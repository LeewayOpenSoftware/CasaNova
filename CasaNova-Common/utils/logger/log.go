package logger

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/samber/lo"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"gopkg.in/natefinch/lumberjack.v2"
)

var loggers *zap.Logger
var once sync.Once

func getFileLogWriter(logPath string, logFileName string, logFileExt string) (writeSyncer zapcore.WriteSyncer) {
	//  lumberjack  log rotate
	lumberJackLogger := &lumberjack.Logger{
		Filename:   filepath.Join(logPath, fmt.Sprintf("%s.%s", logFileName, logFileExt)),
		MaxSize:    10,
		MaxBackups: 60,
		MaxAge:     1,
		Compress:   true,
	}

	return zapcore.AddSync(lumberJackLogger)
}

func LogInitWithWriterSyncers(syncers ...zapcore.WriteSyncer) {
	encoder := getEncoder()
	loggers = zap.New(
		zapcore.NewTee(
			lo.Map(
				syncers,
				func(syncer zapcore.WriteSyncer, index int) zapcore.Core {
					return zapcore.NewCore(encoder, syncer, zapcore.InfoLevel)
				})...,
		),
	)
}

// for unit tests
func LogInitConsoleOnly() {
	once.Do(func() {
		LogInitWithWriterSyncers(
			zapcore.AddSync(os.Stdout),
		)
	})
}

// LogInit initializes the package-level logger. Wave 33-A MEDIUM:
// previously this function unconditionally re-assigned `loggers` every
// time it was called, while `Info`/`Error` read the same global without
// any synchronization. Two services in the same process (the daemon's
// main + a goroutine that ran an embedded library which also called
// LogInit) could race and crash with a nil-deref. Worse, if any goroutine
// called Info before *any* LogInit had run, it would panic on `loggers.Info`
// against a nil pointer. Use the same `once` that LogInitConsoleOnly
// already protects, and harden Info/Error against the no-init case.
func LogInit(logPath string, logFileName string, logFileExt string) {
	once.Do(func() {
		LogInitWithWriterSyncers(
			zapcore.AddSync(os.Stdout),
			getFileLogWriter(logPath, logFileName, logFileExt),
		)
	})
}

func Info(message string, fields ...zap.Field) {
	if loggers == nil {
		return
	}
	callerFields := getCallerInfoForLog()
	fields = append(fields, callerFields...)
	loggers.Info(message, fields...)
}

func Error(message string, fields ...zap.Field) {
	if loggers == nil {
		return
	}
	callerFields := getCallerInfoForLog()
	fields = append(fields, callerFields...)
	loggers.Error(message, fields...)
}

func getCallerInfoForLog() (callerFields []zap.Field) {
	pc, file, line, ok := runtime.Caller(2) //
	if !ok {
		return
	}
	funcName := runtime.FuncForPC(pc).Name()
	funcName = path.Base(funcName) // Base

	callerFields = append(callerFields, zap.String("func", funcName), zap.String("file", file), zap.Int("line", line))
	return
}

func getEncoder() zapcore.Encoder {
	encoderConfig := zap.NewProductionEncoderConfig()
	encoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	return zapcore.NewConsoleEncoder(encoderConfig)
}
