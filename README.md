
# Victoria Logger

This library provides a hook for [Logrus](https://github.com/sirupsen/logrus) to send log entries to VictoriaLogs.

## Installation

To install the library, use `go get`:

```
go get github.com/quiknode-labs/victoria-logger
```


## Usage

Here's a basic example to set up the VictoriaLogs hook:

```go
import "github.com/quiknode-labs/victoria-logger"

func main() {
    streams := make(map[string]interface{})
    streams["service"] = "my-service"
    streams["env"] = "production"

    logger.Init(context.Background(), "http://my.victoria.logs", 2*time.Second, 100, 3, 1*time.Second, streams)
    defer logger.Close()

    logger.Log.Info("This is an example log")
}
```

## Configuration

You can configure the logger with various settings. Check the `logger.go` file for more details and documentation.

## Contributions

Feel free to fork, modify, and submit pull requests. For major changes, please open an issue first to discuss your proposed changes.

## License

This project is open-source and available under the MIT License.
