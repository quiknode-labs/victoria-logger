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

// ... [rest of the code]
