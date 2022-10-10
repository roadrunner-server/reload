package reload

import (
	"os"
	"strings"
	"time"

	endure "github.com/roadrunner-server/endure/pkg/container"
	"github.com/roadrunner-server/errors"
	"go.uber.org/zap"
)

// PluginName contains default plugin name.
const (
	PluginName          string = "reload"
	thresholdChanBuffer uint   = 1000
)

type Plugin struct {
	cfg        *Config
	log        *zap.Logger
	resettable map[string]Resetter

	watcher  *Watcher
	services map[string]interface{}
	stopc    chan struct{}
}

// Resetter interface
type Resetter interface {
	// Reset reload plugin
	Reset() error
}

type Configurer interface {
	// UnmarshalKey takes a single key and unmarshal it into a Struct.
	UnmarshalKey(name string, out any) error

	// Has checks if config section exists.
	Has(name string) bool
}

// Init controller service
func (p *Plugin) Init(cfg Configurer, log *zap.Logger) error {
	const op = errors.Op("reload_plugin_init")
	if !cfg.Has(PluginName) {
		return errors.E(op, errors.Disabled)
	}

	err := cfg.UnmarshalKey(PluginName, &p.cfg)
	if err != nil {
		// disable plugin in case of error
		return errors.E(op, errors.Disabled, err)
	}

	p.cfg.InitDefaults()

	p.log = log
	p.stopc = make(chan struct{}, 1)
	p.services = make(map[string]interface{})

	configs := make([]WatcherConfig, 0, len(p.cfg.Plugins))

	for serviceName, serviceConfig := range p.cfg.Plugins {
		ignored, errIgn := ConvertIgnored(serviceConfig.Ignore)
		if errIgn != nil {
			return errors.E(op, err)
		}
		configs = append(configs, WatcherConfig{
			ServiceName: serviceName,
			Recursive:   serviceConfig.Recursive,
			Directories: serviceConfig.Dirs,
			FilterHooks: func(filename string, patterns []string) error {
				for i := 0; i < len(patterns); i++ {
					if strings.Contains(filename, patterns[i]) {
						return nil
					}
				}
				return errors.E(op, errors.SkipFile)
			},
			Files:        make(map[string]os.FileInfo),
			Ignored:      ignored,
			FilePatterns: append(serviceConfig.Patterns, p.cfg.Patterns...),
		})
	}

	p.watcher, err = NewWatcher(configs, p.log)
	if err != nil {
		return errors.E(op, err)
	}

	p.resettable = make(map[string]Resetter, 2)

	return nil
}

func (p *Plugin) Serve() chan error { //nolint:gocognit
	const op = errors.Op("reload_plugin_serve")
	errCh := make(chan error, 1)
	if p.cfg.Interval < time.Second {
		errCh <- errors.E(op, errors.Str("reload interval is too fast"))
		return errCh
	}

	// make a map with unique services
	// so, if we would have 100 events from http service
	// in map we would see only 1 key, and it's config
	thCh := make(chan struct {
		serviceConfig ServiceConfig
		plugin        string
	}, thresholdChanBuffer)

	// use the same interval
	timer := time.NewTimer(p.cfg.Interval)

	go func() {
		for e := range p.watcher.Event {
			thCh <- struct {
				serviceConfig ServiceConfig
				plugin        string
			}{serviceConfig: p.cfg.Plugins[e.plugin], plugin: e.plugin}
		}
	}()

	// map with config by services
	updated := make(map[string]ServiceConfig, len(p.cfg.Plugins))

	go func() {
		for {
			select {
			case cfg := <-thCh:
				// logic is following:
				// restart
				timer.Stop()
				// replace previous value in map by more recent without adding new one
				updated[cfg.plugin] = cfg.serviceConfig
				// if we are getting a lot of events, we shouldn't restart particular service on each of it (user doing batch move or very fast typing)
				// instead, we are resetting the timer and wait for s.cfg.Interval time
				// If there is no more events, we restart service only once
				timer.Reset(p.cfg.Interval)
			case <-timer.C:
				if len(updated) > 0 {
					for name := range updated {
						if _, ok := p.resettable[name]; ok {
							err := p.resettable[name].Reset()
							if err != nil {
								p.log.Error("failed to allocate a worker, RR will try to allocate a worker on the following change")
							}
						}
					}
					// zero map
					updated = make(map[string]ServiceConfig, len(p.cfg.Plugins))
				}
			case <-p.stopc:
				timer.Stop()
				return
			}
		}
	}()

	go func() {
		err := p.watcher.StartPolling(p.cfg.Interval)
		if err != nil {
			errCh <- errors.E(op, err)
			return
		}
	}()

	return errCh
}

func (p *Plugin) Stop() error {
	p.watcher.Stop()
	p.stopc <- struct{}{}
	return nil
}

func (p *Plugin) Collects() []interface{} {
	return []interface{}{
		p.CollectResettable,
	}
}

func (p *Plugin) CollectResettable(name endure.Named, r Resetter) {
	p.resettable[name.Name()] = r
}

func (p *Plugin) Name() string {
	return PluginName
}
