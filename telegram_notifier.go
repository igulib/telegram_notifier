package telegram_notifier

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/igulib/app"
	"github.com/rs/zerolog"

	"github.com/nikoksr/notify"
	"github.com/nikoksr/notify/service/telegram"
)

var (

	// DefaultSendTimeoutSec is the default timeout in seconds
	// to send a telegram message.
	DefaultSendTimeoutSec = 5

	// DefaultMsgBufSize is the default message buffer size for
	// TelegramMessage channel.
	DefaultMsgBufSize = 50
)

// Errors
var (
	ErrUnitNotAvailable = errors.New("unit not available")

	ErrBadLogLevel = errors.New("bad log level")

	ErrBadTelegramBotToken = errors.New("bad telegram bot token")

	ErrBadTelegramChatId = errors.New("bad telegram chat ID")

	ErrLogTelegramConfigIsNil = errors.New("log telegram config is nil")
)

// Internal variables

var (
	allowedLogLevels = map[string]zerolog.Level{
		"":         zerolog.DebugLevel,
		"disabled": zerolog.Disabled,
		"trace":    zerolog.TraceLevel,
		"debug":    zerolog.DebugLevel,
		"info":     zerolog.InfoLevel,
		"warning":  zerolog.WarnLevel,
		"error":    zerolog.ErrorLevel,
		"fatal":    zerolog.FatalLevel,
		"panic":    zerolog.PanicLevel,
	}
)

type Config struct {
	// BotToken specifies the Telegram bot secret token.
	BotToken string `yaml:"bot_token" json:"bot_token"`

	// BotTokenEnvVar specifies the name of the environment variable
	// that contains BotToken for current telegram notifier.
	// This allows for more versatile and secure configuration.
	// The environment variable has precedence over the BotToken value.
	BotTokenEnvVar string `yaml:"bot_token_env_var" json:"bot_token_env_var"`

	// ChatIds specifies the receivers of notifications.
	ChatIds []int64 `yaml:"chat_ids" json:"chat_ids"`

	// ChatIdsEnvVar specifies the name of the environment variable
	// that contains comma-separated ChatIds for current telegram notifier.
	ChatIdsEnvVar string `yaml:"chat_ids_env_var" json:"chat_ids_env_var"`

	// Fields required for integration with `igulib/app_logger`

	// LogLevels define the log levels the messages must have to be send to Telegram
	// when integrated with `igulib/app_logger`.
	// If none specified, no messages will be sent via Telegram.
	LogLevels []string `yaml:"log_levels" json:"log_levels"`

	// LogOnlyWithPrefixes defines the prefixes that a log message must start with
	// in order to be sent to Telegram chats.
	// If a message starts with any of these prefixes, it will be sent via Telegram.
	// If prefixes not specified, all messages with the appropriate log level
	// will be sent.
	// This setting only has effect for log messages from `igulib/app_logger`
	// when integrated with it via zerolog.Hook.
	LogOnlyWithPrefixes []string `yaml:"log_only_with_prefixes" json:"log_only_with_prefixes"`

	// LogDateTime enables appending date and time to the log message.
	LogDateTime bool `yaml:"log_date_time" json:"log_date_time"`

	// LogUseUTC enables UTC time instead of local if LogDateTime is true.
	LogUseUTC bool `yaml:"log_use_utc" json:"log_use_utc"`
}

type validatedConfig struct {
	BotToken            string
	ChatIds             []int64
	LogLevels           []zerolog.Level
	LogMustHavePrefixes []string
	LogDateTime         bool
	LogUseUTC           bool
}

func ParseYamlConfig(data []byte) (*Config, error) {
	config := &Config{}
	err := yaml.Unmarshal(data, config)
	return config, err
}

// Parse chat_ids provided via environment variable.
// This must be either a single id or a comma-separated list.
func parseChatIds(chatIds string) ([]int64, error) {
	r := make([]int64, 0)
	s := strings.Split(chatIds, ",")
	for _, idString := range s {
		idString = strings.TrimSpace(idString)
		id, err := strconv.ParseInt(idString, 10, 64)
		if err != nil {
			return r, err
		}
		r = append(r, id)
	}
	return r, nil
}

func validateConfig(c *Config) (*validatedConfig, error) {
	v := &validatedConfig{
		ChatIds:             make([]int64, 0),
		LogLevels:           make([]zerolog.Level, 0),
		LogMustHavePrefixes: make([]string, 0),
	}
	if c == nil {
		return v, ErrLogTelegramConfigIsNil
	}

	// Bot token (env var has precedence)
	var botToken string
	if c.BotTokenEnvVar != "" {
		botToken = os.Getenv(c.BotTokenEnvVar)
	}

	if botToken == "" {
		if c.BotToken == "" {
			return v, ErrBadTelegramBotToken
		}
		v.BotToken = c.BotToken
	} else {
		v.BotToken = botToken
	}

	var chatIds string
	if c.BotTokenEnvVar != "" {
		chatIds = os.Getenv(c.ChatIdsEnvVar)
	}

	if chatIds == "" {
		if len(c.ChatIds) == 0 {
			return v, ErrBadTelegramChatId
		}
		v.ChatIds = append(v.ChatIds, c.ChatIds...)
	} else {
		var err error
		v.ChatIds, err = parseChatIds(chatIds)
		if err != nil {
			return v, fmt.Errorf("failed to parse chat_ids provided via environment variable %q: %w", c.ChatIdsEnvVar, err)
		}
	}

	// LogLevels
	for _, l := range c.LogLevels {
		l = strings.TrimSpace(l)
		l = strings.ToLower(l)
		parsedLevel, ok := allowedLogLevels[l]
		if !ok {
			return v, ErrBadLogLevel
		}
		v.LogLevels = append(v.LogLevels, parsedLevel)
	}

	v.LogMustHavePrefixes = append(v.LogMustHavePrefixes, c.LogOnlyWithPrefixes...)

	// Fields that do not require validation
	v.LogDateTime = c.LogDateTime
	v.LogUseUTC = c.LogUseUTC

	return v, nil
}

type TelegramMessage struct {
	Title string
	Text  string
}

// TelegramNotifier unit. Do not instantiate TelegramNotifier directly,
// use the New function instead.
type TelegramNotifier struct {
	unitRunner *app.UnitLifecycleRunner

	availability     app.UnitAvailability
	availabilityLock sync.Mutex

	config *validatedConfig

	// Telegram service
	logMessageTitleSuffix string
	tgServiceRunning      atomic.Bool
	tgRequestCounter      sync.WaitGroup
	tgMsgChan             chan TelegramMessage
	tgServiceQuitRequest  chan struct{}
	tgServiceDone         chan struct{}
}

// New creates a new TelegramNotifier unit.
// This function should be used instead of direct construction of TelegramNotifier.
func New(unitName string, c *Config) (*TelegramNotifier, error) {

	u := &TelegramNotifier{
		unitRunner: app.NewUnitLifecycleRunner(unitName),
	}

	u.unitRunner.SetOwner(u)

	err := u.init(c)
	if err != nil {
		return u, err
	}

	return u, nil
}

// AddNew creates a new TelegramNotifier unit and
// adds it into the default app unit manager (app.M).
// This function should be used instead of direct construction of TelegramNotifier.
func AddNew(unitName string, c *Config) (*TelegramNotifier, error) {
	u, err := New(unitName, c)
	if err != nil {
		return u, err
	}
	return u, app.M.AddUnit(u)
}

func (u *TelegramNotifier) init(c *Config) error {

	// Validate configuration
	vc, err := validateConfig(c)
	if err != nil {
		return err
	}
	u.config = vc

	u.tgMsgChan = make(chan TelegramMessage, DefaultMsgBufSize)

	return nil
}

// SetLogMessageTitleSuffix sets an optional suffix to
// the log message title when log messages are forwarded from `igulib/app_logger`.
// This method has no effect if current `TelegramNotifier` not used as a hook
// at `igulib/app_logger`.
// Message title example without suffix: `ERROR`, with suffix: `ERROR | my-suffix`.
func (u *TelegramNotifier) SetLogMessageTitleSuffix(appName string) {
	u.logMessageTitleSuffix = appName
}

// Run implements zerolog.Hook.
func (u *TelegramNotifier) Run(
	e *zerolog.Event,
	level zerolog.Level,
	message string,
) {
	// Check if message has required log level
	levelOk := false
	for _, allowedLevel := range u.config.LogLevels {
		if level == allowedLevel {
			levelOk = true
			break
		}
	}

	if !levelOk {
		return
	}

	// Check message prefix
	if len(u.config.LogMustHavePrefixes) > 0 {
		prefixFound := false
		for _, p := range u.config.LogMustHavePrefixes {
			if strings.HasPrefix(message, p) {
				prefixFound = true
				break
			}
		}
		if !prefixFound {
			return
		}
	}

	var title string
	switch level {
	case zerolog.NoLevel:
		title = "LOG MESSAGE"
	case zerolog.TraceLevel:
		title = "TRACE"
	case zerolog.DebugLevel:
		title = "DEBUG"
	case zerolog.InfoLevel:
		title = "INFO"
	case zerolog.WarnLevel:
		title = "WARNING"
	case zerolog.ErrorLevel:
		title = "ERROR"
	case zerolog.FatalLevel:
		title = "FATAL"
	case zerolog.PanicLevel:
		title = "PANIC"
	}
	if u.logMessageTitleSuffix != "" {
		title = fmt.Sprintf("%s | %s", title, u.logMessageTitleSuffix)
	}

	if u.config.LogDateTime {
		if u.config.LogUseUTC {
			message = fmt.Sprintf("%s | %s", message, time.Now().UTC().Format(time.RFC3339))
		} else {
			message = fmt.Sprintf("%s | %s", message, time.Now().Format(time.RFC3339))
		}

	}

	err := u.SendAsync(title, message)
	if err != nil {
		// Do not use logger here to prevent positive feedback
		fmt.Fprintf(os.Stderr, "(%s) failed to send message", u.unitRunner.Name())
	}

}

// SendAsync asynchronously sends the message via Telegram, it is thread-safe.
func (u *TelegramNotifier) SendAsync(title, text string) error {

	u.availabilityLock.Lock()
	if u.availability == app.UAvailable {
		u.tgRequestCounter.Add(1)
		u.tgMsgChan <- TelegramMessage{title, text}
		u.availabilityLock.Unlock()
		return nil
	}
	u.availabilityLock.Unlock()
	return ErrUnitNotAvailable
}

// UnitStart implements app.IUnit.
func (u *TelegramNotifier) UnitStart() app.UnitOperationResult {
	swapped := u.tgServiceRunning.CompareAndSwap(false, true)
	if swapped {
		u.tgServiceQuitRequest = make(chan struct{})
		u.tgServiceDone = make(chan struct{})
		u.availabilityLock.Lock()
		u.availability = app.UAvailable
		u.availabilityLock.Unlock()
		go u.telegramService()
	}

	r := app.UnitOperationResult{
		OK: true,
	}
	return r
}

// UnitPause implements app.IUnit.
func (u *TelegramNotifier) UnitPause() app.UnitOperationResult {

	u.availabilityLock.Lock()
	u.availability = app.UTemporarilyUnavailable
	u.availabilityLock.Unlock()

	r := app.UnitOperationResult{
		OK: true,
	}
	return r
}

// UnitQuit implements app.IUnit.
func (u *TelegramNotifier) UnitQuit() app.UnitOperationResult {
	u.availabilityLock.Lock()
	u.availability = app.UNotAvailable
	u.availabilityLock.Unlock()

	// Wait until all ongoing requests complete
	u.tgRequestCounter.Wait()

	// Shut down telegram service if running
	swapped := u.tgServiceRunning.CompareAndSwap(true, false)
	if swapped {
		close(u.tgServiceQuitRequest)
		// Wait until telegram service goroutine exits
		<-u.tgServiceDone
	}

	r := app.UnitOperationResult{
		OK: true,
	}
	return r
}

// UnitRunner implements app.IUnit.
func (u *TelegramNotifier) UnitRunner() *app.UnitLifecycleRunner {
	return u.unitRunner
}

// UnitAvailability implements app.IUnit.
func (u *TelegramNotifier) UnitAvailability() app.UnitAvailability {
	return u.availability
}

// This method should only be called from UnitStart method with proper synchronization.
func (u *TelegramNotifier) telegramService() {
	defer close(u.tgServiceDone)

	telegramService, err := telegram.New(
		u.config.BotToken,
	)

	if err != nil {
		u.availabilityLock.Lock()
		u.availability = app.UNotAvailable
		u.availabilityLock.Unlock()
		return
	}

	telegramService.AddReceivers(u.config.ChatIds...)

	notifier := notify.New()

	notifier.UseServices(telegramService)

	for {
		select {
		case msg := <-u.tgMsgChan:
			// Process each request in a separate goroutine
			go func() {
				defer u.tgRequestCounter.Done()

				ctx, cancel := context.WithTimeout(
					context.Background(),
					time.Duration(DefaultSendTimeoutSec)*time.Second,
				)

				defer cancel()

				err = notifier.Send(ctx, msg.Title, msg.Text)
				if err != nil {
					// Do not use log here to avoid positive feedback.
					fmt.Fprintf(os.Stderr, "(%s) failed to send message", u.unitRunner.Name())
				}
			}()

		case <-u.tgServiceQuitRequest:
			return
		}
	}
}
