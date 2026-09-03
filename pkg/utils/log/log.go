package log

import (
	"os"
	"sync"

	"github.com/natefinch/lumberjack"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

type OutFile struct {
	Enabled    bool   `mapstructure:"enabled" json:"enabled" yaml:"enabled"`
	OutputPath string `mapstructure:"output_path" json:"output_path" yaml:"output_path"`
	MaxSize    int    `mapstructure:"max_size" json:"max_size" yaml:"max_size"`
	MaxAge     int    `mapstructure:"max_age" json:"max_age" yaml:"max_age"`
	MaxBackups int    `mapstructure:"max_backups" json:"max_backups" yaml:"max_backups"`
	Compress   bool   `mapstructure:"compress" json:"compress" yaml:"compress"`
	LocalTime  bool   `mapstructure:"local_time" json:"local_time" yaml:"local_time"`
}

type LogsType string

const (
	JSONLogType LogsType = "json"
	TextLogType LogsType = "text"
)

type Config struct {
	Console   bool    `json:"console" yaml:"console" mapstructure:"console"`
	AddSource bool    `json:"add_source" yaml:"add_source" mapstructure:"add_source"`
	Level     string  `json:"level" yaml:"level" mapstructure:"level"`
	Format    LogsType `json:"format" yaml:"format" mapstructure:"format"`
	Rotate    OutFile `json:"rotate" yaml:"rotate" mapstructure:"rotate"`
}

var (
	logger *zap.Logger
	once   sync.Once
)

func Init(c Config) {
	once.Do(func() {
		cfg := zap.NewProductionEncoderConfig()
		cfg.TimeKey = "dt"
		cfg.MessageKey = "msg"
		cfg.EncodeTime = zapcore.RFC3339NanoTimeEncoder
		encoder := zapcore.NewJSONEncoder(cfg)
		if c.Format != JSONLogType {
			encoder = zapcore.NewConsoleEncoder(cfg)
		}
		level := zap.InfoLevel
		_ = level.Set(c.Level)
		if c.Level == "" {
			level = zap.InfoLevel
		}
		var writers []zapcore.WriteSyncer
		if c.Console || !c.Rotate.Enabled {
			writers = append(writers, zapcore.AddSync(os.Stdout))
		}
		if c.Rotate.Enabled && c.Rotate.OutputPath != "" {
			lj := &lumberjack.Logger{
				Filename:   c.Rotate.OutputPath,
				MaxSize:    c.Rotate.MaxSize,
				MaxAge:     c.Rotate.MaxAge,
				MaxBackups: c.Rotate.MaxBackups,
				Compress:   c.Rotate.Compress,
				LocalTime:  c.Rotate.LocalTime,
			}
			if lj.MaxSize <= 0 {
				lj.MaxSize = 100
			}
			writers = append(writers, zapcore.AddSync(lj))
		}
		core := zapcore.NewCore(encoder, zapcore.NewMultiWriteSyncer(writers...), level)
		opts := []zap.Option{zap.AddCallerSkip(0)}
		if c.AddSource {
			opts = append(opts, zap.AddCaller())
		}
		logger = zap.New(core, opts...)
	})
}

func Logger() *zap.Logger {
	if logger == nil {
		return zap.New(zapcore.NewCore(
			zapcore.NewConsoleEncoder(zap.NewProductionEncoderConfig()),
			zapcore.AddSync(os.Stdout),
			zap.InfoLevel,
		))
	}
	return logger
}

func Debug(msg string, fields ...zapcore.Field) { Logger().Debug(msg, fields...) }
func Info(msg string, fields ...zapcore.Field)  { Logger().Info(msg, fields...) }
func Warn(msg string, fields ...zapcore.Field)  { Logger().Warn(msg, fields...) }
func Error(msg string, fields ...zapcore.Field) { Logger().Error(msg, fields...) }
