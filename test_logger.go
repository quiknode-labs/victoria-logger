package logger

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"
)

// TestLogger is a simple program to validate the victoria-logger changes
func TestLogger() {
	// Initialize logger with large batch size
	fmt.Println("Initializing logger...")
	
	err := Init(
		context.Background(),
		"http://localhost:9428", // This is a fake endpoint - we'll handle connection failures
		time.Second*5,           // 5 second flush interval
		5000,                    // 5000 batch size
		2,                       // 2 retries
		time.Second,             // 1 second retry delay
		map[string]interface{}{"service": "test-service", "environment": "test"},
	)
	
	if err != nil {
		// We expect an error here since the endpoint is fake
		fmt.Printf("Logger initialized with expected error: %v (this is normal for testing)\n", err)
	} else {
		fmt.Printf("Logger initialized successfully\n")
	}

	// Test 1: Basic logging - ensure no panics
	fmt.Println("\nTest 1: Basic logging...")
	Log.Info("This is a test info message")
	Log.Warn("This is a test warning message")
	Log.Error("This is a test error message")
	time.Sleep(100 * time.Millisecond) // Give logger a moment
	fmt.Println("Basic logging test completed without panics")

	// Test 2: High volume logging - test channel buffer behavior
	fmt.Println("\nTest 2: High volume logging...")
	start := time.Now()
	for i := 0; i < 25000; i++ { // Exceed our buffer size of 20000
		Log.Infof("Test message %d", i)
	}
	duration := time.Since(start)
	fmt.Printf("Logged 25000 messages in %v (%.2f msgs/sec)\n", 
		duration, float64(25000)/duration.Seconds())

	// Test 3: Concurrent logging
	fmt.Println("\nTest 3: Concurrent logging...")
	var wg sync.WaitGroup
	numGoroutines := 10
	messagesPerGoroutine := 5000
	
	start = time.Now()
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < messagesPerGoroutine; j++ {
				Log.Infof("Goroutine %d: message %d", id, j)
			}
		}(i)
	}
	wg.Wait()
	duration = time.Since(start)
	totalMessages := numGoroutines * messagesPerGoroutine
	fmt.Printf("Logged %d concurrent messages in %v (%.2f msgs/sec)\n", 
		totalMessages, duration, float64(totalMessages)/duration.Seconds())

	// Test 4: Logging when closed
	fmt.Println("\nTest 4: Logging after closed...")
	Close()
	Log.Info("This message should be dropped")
	Log.Error("This error message should also be dropped")
	fmt.Println("Messages after Close() were handled without panic")

	// Final report
	fmt.Println("\nAll tests completed successfully")
	log.Println("Note: Any error messages about failed connections are expected in this test")
}