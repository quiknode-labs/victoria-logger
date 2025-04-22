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
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/segmentio/encoding/json"

	retry_v4 "github.com/avast/retry-go/v4"
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
// ChannelBufferSize controls the size of the internal log channel buffer
// Setting this to a much larger value (20000) to avoid blocking
var ChannelBufferSize = 20000

func Init(ctx context.Context, url string, flushInterval time.Duration, batchSize, maxRetries int, retryDelay time.Duration, streams map[string]interface{}) error {
	var initErr error
	once.Do(func() { // Ensure initialization only happens once
		// Configure logger to drop debug logs in production mode
		baseLogger := logrus.New()
		if streams["environment"] == "production" || streams["environment"] == "staging" {
			// In production/staging, default to a higher log level to reduce volume
			baseLogger.SetLevel(logrus.InfoLevel)
		}

		if ctx == nil {
			ctx = context.Background()
		}
		Log = baseLogger.WithContext(ctx)
		
		// Validate inputs
		if url == "" {
			initErr = errors.New("url cannot be empty")
			return
		}
		if flushInterval <= 0 {
			initErr = errors.New("flushInterval must be greater than 0")
			return
		}
		if batchSize <= 0 {
			initErr = errors.New("batchSize must be greater than 0")
			return
		}
		if maxRetries < 0 {
			initErr = errors.New("maxRetries cannot be negative")
			return
		}
		if retryDelay <= 0 {
			initErr = errors.New("retryDelay must be greater than 0")
			return
		}
		if len(streams) == 0 {
			initErr = errors.New("streams map cannot be empty")
			return
		}
		
		Log = baseLogger.WithFields(streams)
		
		// Create an HTTP client with a shorter timeout to avoid blocking
		client := &http.Client{
			Timeout: 3 * time.Second, // Reduced from 5s to 3s
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 100,
				IdleConnTimeout:     90 * time.Second,
			},
		}
		
		// Perform a health check to ensure VictoriaLogs is operational
		// but don't block initialization if it fails - just log a warning
		go func() {
			if err := healthCheck(url); err != nil {
				logrus.Warnf("VictoriaLogs health check failed: %v - logging will continue locally", err)
			}
		}()
		
		// Use string builder to build the stream fields
		streamsFieldsBuilder := strings.Builder{}
		for key := range streams {
			streamsFieldsBuilder.WriteString(key)
			streamsFieldsBuilder.WriteString(",")
		}
		// Remove the last comma
		streamsFields := strings.TrimSuffix(streamsFieldsBuilder.String(), ",")
		
		vlHook := &victoriaLogsHook{
			URL:          url + "/insert/jsonline",
			streamFields: streamsFields,
			batchSize:    batchSize,
			maxRetries:   maxRetries,
			retryDelay:   retryDelay,
			client:       client,
		}

		baseLogger.AddHook(vlHook)
		
		// Initialize channels with generous buffer sizes to avoid blocking
		flushSignalCh = make(chan bool, 10)    // Multiple flush signals can be queued
		stopCh = make(chan bool, 1)            // Only one stop signal needed
		ticker = time.NewTicker(flushInterval)
		
		// Use the larger buffer size for the main log channel
		// This is the key change to prevent blocking on high log volume
		ch = make(chan *logrus.Entry, ChannelBufferSize) 
		
		baseLogger.WithContext(ctx).WithFields(streams)
		wg = sync.WaitGroup{}

		wg.Add(1)
		go vlHook.flusher()
	})
	return initErr
}

// Levels returns all log levels to ensure the hook is used for all log levels.
func (hook *victoriaLogsHook) Levels() []logrus.Level {
	return logrus.AllLevels
}

// Fire queues the log entry for batching.
func (hook *victoriaLogsHook) Fire(entry *logrus.Entry) error {
	if atomic.LoadInt32(&closed) == 1 {
		// Silently drop logs after closure
		return nil
	}
	
	// Create a copy of the entry to avoid race conditions
	entryCopy := &logrus.Entry{
		Logger:  entry.Logger,
		Data:    make(logrus.Fields, len(entry.Data)),
		Time:    entry.Time,
		Level:   entry.Level,
		Message: entry.Message,
	}
	for k, v := range entry.Data {
		entryCopy.Data[k] = v
	}
	
	select {
	case ch <- entryCopy:
		// Successfully sent the entry to the channel
		return nil
	default:
		// Channel is full, but we don't want to block
		// Signal the flusher and drop the log entry
		select {
		case flushSignalCh <- true:
			// Successfully signaled the flusher
		default:
			// Flusher is already busy, just drop the log entry
		}
		
		// For error and fatal levels, make a blocking attempt as these are important
		if entry.Level <= logrus.ErrorLevel {
			select {
			case ch <- entryCopy:
				// Added error/fatal log to channel
			default:
				// Even the error log couldn't be added, log locally
				logrus.Warnf("Dropped important log (level=%s): %s", entry.Level, entry.Message)
			}
		}
		return nil
	}
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
// Preallocated buffer pool for JSON serialization
var bufferPool = sync.Pool{
	New: func() interface{} {
		return new(bytes.Buffer)
	},
}

// mapPool for reusing maps during log serialization
var mapPool = sync.Pool{
	New: func() interface{} {
		return make(map[string]interface{}, 16) // Reasonable initial capacity
	},
}

func (hook *victoriaLogsHook) flush(batch []*logrus.Entry) {
	if len(batch) == 0 {
		return
	}

	// Get buffer from pool
	buffer := bufferPool.Get().(*bytes.Buffer)
	buffer.Reset()
	defer bufferPool.Put(buffer)

	// Prepare entries for sending
	for _, entry := range batch {
		// Get a logData map from the pool
		logData := mapPool.Get().(map[string]interface{})
		for k := range logData {
			delete(logData, k) // Clear the map for reuse
		}

		// Add standard fields
		logData["_msg"] = entry.Message
		logData["_time"] = entry.Time.Format(time.RFC3339Nano)
		logData["level"] = entry.Level.String()

		// Add custom fields
		for k, v := range entry.Data {
			logData[k] = v
		}

		// Marshal to JSON
		jsonEntry, err := json.Marshal(logData)
		mapPool.Put(logData) // Return map to pool

		if err != nil {
			// Don't log here to avoid recursion - just skip the entry
			continue
		}

		buffer.Write(jsonEntry)
		buffer.Write([]byte("\n"))
	}

	// If buffer is empty after processing (all entries failed to marshal), return early
	if buffer.Len() == 0 {
		return
	}

	// Create a context with a shorter timeout to avoid long blocks
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// Send logs with retry logic but limit total time
	err := retry_v4.Do(
		func() error {
			req, err := http.NewRequestWithContext(
				ctx,
				"POST",
				hook.URL+fmt.Sprintf("?_stream_fields=%s", hook.streamFields),
				buffer,
			)
			if err != nil {
				// Local silent error
				return err
			}
			req.Header.Set("Content-Type", "application/stream+json")

			resp, err := hook.client.Do(req)
			if err != nil {
				// Check if context is canceled or deadline exceeded
				if ctx.Err() != nil {
					return retry_v4.Unrecoverable(ctx.Err()) // Stop retrying
				}
				return err
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("status: %s", resp.Status)
			}
			return nil
		},
		retry_v4.Attempts(uint(hook.maxRetries)),
		retry_v4.DelayType(retry_v4.BackOffDelay), // Use backoff for more efficient retries
		retry_v4.MaxDelay(200*time.Millisecond),   // Cap retry delays
		retry_v4.LastErrorOnly(true),              // Only log the last error
	)

	// Silently handle errors - we don't want logging errors to cascade
	if err != nil && ctx.Err() == nil {
		// Only log final error once if it's not due to context timeout
		logrus.Warnf("Failed to send %d logs to VictoriaLogs", len(batch))
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

// Define a private type to prevent context key collisions
type logContextKey struct{}

// logKey is the key for storing the logger in the context
var logKey = logContextKey{}

// WithLog embeds the logg into the context
func WithLog(ctx context.Context, logger *logrus.Entry) context.Context {
	return context.WithValue(ctx, logKey, logger)
}

// LogFromContext retrieves the log from the context
func LogFromContext(ctx context.Context) *logrus.Entry {
	logger, ok := ctx.Value(logKey).(*logrus.Entry)
	if !ok {
		// Return a default logger if none is found in the context
		return Log
	}
	return logger
}
