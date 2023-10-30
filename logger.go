
// Package logger provides a hook for Logrus to send log entries to VictoriaLogs.
// To use this package, initialize it with the desired configurations, and then use
// the provided global logger instance for logging.
//
// Example:
//
//	logger.Init("http://my.victoria.logs", "production", "my-service", 2*time.Second, 100, 3, 1*time.Second)
//	defer logger.Close() // Call this before exiting to ensure all logs are sent
//	logger.Log.Info("This is an example log")
package logger

import (
	"bytes"
	"context"
	"github.com/segmentio/encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/avast/retry-go"
	"github.com/sirupsen/logrus"
)

// VictoriaLogsHook is a Logrus hook designed to forward log entries to VictoriaLogs.
type victoriaLogsHook struct {
	URL          string // The URL endpoint for VictoriaLogs
	streamFields string
	batchSize    int           // Number of logs to batch before sending
	maxRetries   int           // Max number of retries for sending logs
	retryDelay   time.Duration // Delay between retries
	client       *http.Client  // HTTP client
}

var (
	flushSignalCh chan bool
	once          sync.Once
	stopCh        chan bool          // Channel to signal stop
	ticker        *time.Ticker       // Ticker for flush intervals
	wg            sync.WaitGroup     // WaitGroup to ensure all logs are sent
	ch            chan *logrus.Entry // Channel for log entries
	closed        int32
)

func init() {
	baseLogger := logrus.New()
	Log = baseLogger.WithContext(context.Background())
}

// Init initializes the global logger with the VictoriaLogsHook.
//
// url: Endpoint for VictoriaLogs without path.
// env: Deployment environment (e.g. "production").
// service: Name of the service/application.
// flushPeriod: Duration to periodically flush logs.
// batchSize: Number of logs to batch before sending.
// maxRetries: Max number of retries for sending logs.
// retryDelay: Delay between retries.
func Init(ctx context.Context, url string, flushInterval time.Duration, batchSize, maxRetries int, retryDelay time.Duration, streams map[string]interface{}) error {
	var initErr error
	once.Do(func() { // Ensure initialization only happens once
		baseLogger := logrus.New()
		if ctx == nil {
			ctx = context.Background()
		}
		Log = baseLogger.WithContext(ctx)
		if url == "" {
			initErr = errors.New("url cannot be empty")
		}
		if flushInterval <= 0 {
			initErr = errors.New("flushInterval must be greater than 0")
		}
		if batchSize <= 0 {
			initErr = errors.New("batchSize must be greater than 0")
		}
		if maxRetries < 0 {
			initErr = errors.New("maxRetries cannot be negative")
		}
		if retryDelay <= 0 {
			initErr = errors.New("retryDelay must be greater than 0")
		}
		if len(streams) == 0 {
			initErr = errors.New("streams map cannot be empty")
		}
		Log = baseLogger.WithFields(streams)
		// Perform a health check to ensure VictoriaLogs is operational
		if err := healthCheck(url); err != nil {
			initErr = fmt.Errorf("health check failed: %w", err)
		}

		client := &http.Client{
			Timeout: 5 * time.Second,
		}
		// use string builder to build the stream fields
		streamsFieldsBuilder := strings.Builder{}

		for key, _ := range streams {
			streamsFieldsBuilder.WriteString(key)
			streamsFieldsBuilder.WriteString(",")
		}
		// remove the last comma
		streamsFields := strings.TrimSuffix(streamsFieldsBuilder.String(), ",")
		vlHook := &victoriaLogsHook{
			URL:          url + "/insert/jsonline",
			streamFields: streamsFields,

			batchSize:  batchSize,
			maxRetries: maxRetries,
			retryDelay: retryDelay,
			client:     client,
		}

		baseLogger.AddHook(vlHook)
		flushSignalCh = make(chan bool)
		stopCh = make(chan bool)
		ticker = time.NewTicker(flushInterval)
		ch = make(chan *logrus.Entry, batchSize)
		baseLogger.WithContext(ctx).WithFields(streams)
		wg = sync.WaitGroup{}

		wg.Add(1)
		go vlHook.flusher()
	})
	return initErr // Return nil on successful initialization
}

// Levels returns all log levels to ensure the hook is used for all log levels.
func (hook *victoriaLogsHook) Levels() []logrus.Level {
	return logrus.AllLevels
}

// Fire queues the log entry for batching.
func (hook *victoriaLogsHook) Fire(entry *logrus.Entry) error {
	if atomic.LoadInt32(&closed) == 1 {
		logrus.Warn("Attempted to log after logger has been closed. Log entry dropped.")
		return nil
	}
	select {
	case ch <- entry:
		// Successfully sent the entry to the channel
	default:
		// Channel is full, signal the flusher to flush
		flushSignalCh <- true
		ch <- entry // You might want to handle the case where this blocks too
	}
	return nil
}

// flusher is a background goroutine that batches log entries and flushes them to VictoriaLogs.
func (hook *victoriaLogsHook) flusher() {
	defer wg.Done()

	batch := make([]*logrus.Entry, 0, hook.batchSize)
	for {
		select {
		case entry, ok := <-ch:
			if !ok {
				hook.flush(batch)
				return
			}
			batch = append(batch, entry)

			if len(batch) >= hook.batchSize {
				hook.flush(batch)
				batch = batch[:0]
			}

		case <-ticker.C:
			hook.flush(batch)
			batch = batch[:0]

		case <-flushSignalCh:
			hook.flush(batch)
			batch = batch[:0]

		case <-stopCh:
			hook.flush(batch)
			return
		}
	}
}

// flush sends the batched log entries to VictoriaLogs.
func (hook *victoriaLogsHook) flush(batch []*logrus.Entry) {
	if len(batch) == 0 {
		return
	}

	var buffer bytes.Buffer
	for _, entry := range batch {
		// Get a logData map from the pool
		logData := make(map[string]interface{}, len(entry.Data)+3)

		logData["_msg"] = entry.Message
		logData["_time"] = entry.Time.Format(time.RFC3339Nano)
		logData["level"] = entry.Level.String()

		for k, v := range entry.Data {
			logData[k] = v
		}

		jsonEntry, err := json.Marshal(logData)
		if err != nil {
			logrus.Errorf("Error converting log entry to string: %v", err)
			continue
		}

		buffer.Write(jsonEntry)
		buffer.Write([]byte("\n"))

	}
	err := retry.Do(
		func() error {
			req, err := http.NewRequest(
				"POST",
				hook.URL+fmt.Sprintf("?_stream_fields=%s", hook.streamFields),
				&buffer,
			)
			if err != nil {
				logrus.Error("Failed to create request:", err)
				return err
			}
			req.Header.Set("Content-Type", "application/stream+json")

			resp, err := hook.client.Do(req)
			if err != nil {
				logrus.Errorf("Failed to send logs: %v", err)
				return err
			}
			defer resp.Body.Close() // Always close the response body

			if resp.StatusCode != http.StatusOK {
				errMsg := fmt.Sprintf("Failed to send logs with status: %s", resp.Status)
				logrus.Error(errMsg)
				return errors.New(errMsg) // Return this error instead of the previous one
			}
			return nil
		},
		retry.Attempts(uint(hook.maxRetries)),
		retry.Delay(hook.retryDelay),
	)
	buffer.Reset()
	if err != nil {
		logrus.Errorln("Failed to send logs after retries:", err)
	}
}

// Close should be called to gracefully shut down the logger.
// It ensures that all logs are sent before exiting.
func Close() {
	atomic.StoreInt32(&closed, 1)
	ticker.Stop()
	close(stopCh)
	close(ch)
	wg.Wait()
}

// Log is the global logger instance.
var Log *logrus.Entry

func healthCheck(url string) error {
	resp, err := http.Get(url + "/metrics")
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return errors.New("VictoriaLogs is not operational")
	}
	return nil
}
