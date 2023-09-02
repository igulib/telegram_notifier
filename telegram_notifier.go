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
	"github.com/rs/zerolog/log"

	"github.com/nikoksr/notify"
	"github.com/nikoksr/notify/service/telegram"
)

var (

	// DefaultSendTimeoutSec is the default timeout in seconds
	// to send a telegram message.
	DefaultSendTimeoutSec = 5

	DefaultMsgBufSize = 50

	DefaultMsgCaption = "Log Message"
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
	BotToken string `yaml:"bot_token" json:"bot_token"`

	// BotTokenEnvVar defines the name of the environment variable
	// that contains BotToken for current telegram notifier.
	// This allows for more versatile and secure configuration.
	// The environment variable has precedence over the BotToken value.
	BotTokenEnvVar string `yaml:"bot_token_env_var" json:"bot_token_env_var"`

	// ChatIds specifies the receivers of notifications.
	ChatIds []int64 `yaml:"chat_ids" json:"chat_ids"`

	ChatIdsEnvVar string `yaml:"chat_ids_env_var" json:"chat_ids_env_var"`

	// Fields required for integration with `igulib/app_logger`

	// LogLevels define the log levels the messages must have to be send to Telegram
	// when integrated with `igulib/app_logger`.
	// If none specified, no messages will be sent to Telegram.
	LogLevels []string `yaml:"log_levels" json:"log_levels"`

	// WithKeys defines the keys that a log message must contain
	// in order to be sent to Telegram (the value is arbitrary).
	// If a message has at least one of these keys, it will be sent to Telegram.
	// If no keys specified, all messages with specified log level
	// will be sent.
	WithKeys []string `yaml:"with_keys" json:"with_keys"`

	// MessageCaption defines the caption of the log message
	// when integrated with `igulib/app_logger`.
	// Message captions are obtained in the following order:
	// values of "caption" or "title" keys of the log message if any,
	// value of MessageCaption configuration field, default value.
	MessageCaption string `yaml:"message_caption" json:"message_caption"`
}

type validatedConfig struct {
	BotToken  string
	ChatIds   []int64
	LogLevels []zerolog.Level
	WithKeys  []string
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
		ChatIds:   make([]int64, 0),
		LogLevels: make([]zerolog.Level, 0),
		WithKeys:  make([]string, 0),
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

	// ChatIds (env var has precedence)

	// if len(c.ChatIds) == 0 {
	// 	return v, ErrBadTelegramChatId
	// }
	// v.ChatIds = append(v.ChatIds, c.ChatIds...)

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

	v.WithKeys = append(v.WithKeys, c.WithKeys...)

	// Fields that do not require validation
	// none

	fmt.Printf("*** VALIDATED CONFIG: %+v\n", v)

	return v, nil
}

type TelegramMessage struct {
	Caption string
	Text    string
}

// TelegramNotifier.
type TelegramNotifier struct {
	unitRunner *app.UnitLifecycleRunner

	availability     app.UnitAvailability
	availabilityLock sync.Mutex

	config *validatedConfig

	// Telegram service
	tgServiceRunning     atomic.Bool
	tgRequestCounter     sync.WaitGroup
	tgMsgChan            chan TelegramMessage
	tgServiceQuitRequest chan struct{}
	tgServiceDone        chan struct{}
}

// NewTelegramNotifier creates a new telegram_notifier instance and
// adds it into the default app unit manager (app.M).
// The unit's name is 'telegram_notifier'.
func NewTelegramNotifier(unitName string, config *Config) (*TelegramNotifier, error) {

	u := &TelegramNotifier{
		unitRunner: app.NewUnitLifecycleRunner(unitName),
	}

	u.unitRunner.SetOwner(u)

	err := u.init(config)
	if err != nil {
		return u, err
	}

	return u, nil
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

	if levelOk {
		// TODO: check keys

		err := u.Send("Log message", message)
		if err != nil {
			log.Error().Err(err).Msgf("(%s) failed to send message", u.unitRunner.Name())
		}
	}
}

func (u *TelegramNotifier) Send(caption, text string) error {

	u.availabilityLock.Lock()
	if u.availability == app.UAvailable {
		u.tgRequestCounter.Add(1)
		u.tgMsgChan <- TelegramMessage{caption, text}
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

				err = notifier.Send(ctx, msg.Caption, msg.Text)
				if err != nil {
					log.Error().Err(err).Msgf("(%s) failed to send message", u.unitRunner.Name())
				}
			}()

		case <-u.tgServiceQuitRequest:
			return
		}
	}
}
