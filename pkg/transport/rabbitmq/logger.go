package rabbitmq

type Logger interface {
	Errorf(format string, args ...interface{})
}
