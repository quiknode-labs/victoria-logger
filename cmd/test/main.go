package main

import (
	"fmt"
	"os"

	logger "github.com/quiknode-labs/squirrel/victoria-logger"
)

func main() {
	fmt.Println("Running victoria-logger test program")
	fmt.Println("===================================")
	
	// Run the test logger
	logger.TestLogger()
	
	fmt.Println("\nTest completed. If you see this message, no panic occurred.")
	os.Exit(0)
}