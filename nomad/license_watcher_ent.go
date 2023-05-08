//go:build ent

package nomad

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/armon/go-metrics"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-licensing/v3"
	nomadLicense "github.com/hashicorp/nomad-licensing/license"
)

const (
	// permanentLicenseID is the license ID used for permanent (s3) enterprise builds
	permanentLicenseID = "permanent"

	licenseExpired = "license is no longer valid"
)

// ServerLicense contains an expanded license and its corresponding blob
type ServerLicense struct {
	license *nomadLicense.License
	blob    string
}

type LicenseWatcher struct {
	// TODO: it might be possible to avoid this atomic now that we've removed
	// raft updates; the whole configuration needs to be updated
	// license is the watchers atomically stored ServerLicense
	licenseInfo atomic.Value

	// fileLicense is the license loaded from the server's license path or env
	// it is set when the LicenseWatcher is initialized and when Reloaded.
	fileLicense string

	watcher *licensing.Watcher

	logMu  sync.Mutex
	logger hclog.Logger

	// logTimes tracks the last time a log message was sent for a feature
	logTimes map[nomadLicense.Features]time.Time
}

func NewLicenseWatcher(logger hclog.Logger, cfg *LicenseConfig) (*LicenseWatcher, error) {
	blob, err := cfg.licenseString()
	if err != nil {
		return nil, err
	}
	if blob == "" {
		return nil, errors.New("failed to read license: license is missing. To add a license, configure \"license_path\" in your server configuration file, use the NOMAD_LICENSE environment variable, or use the NOMAD_LICENSE_PATH environment variable. For a trial license of Nomad Enterprise, visit https://nomadproject.io/trial.")
	}

	// allowing unset BuildDate would mean licenses effectively never expire.
	if cfg.BuildDate.IsZero() {
		return nil, errors.New("error unset BuildDate")
	}

	lw := &LicenseWatcher{
		fileLicense: blob,
		logger:      logger.Named("licensing"),
		logTimes:    make(map[nomadLicense.Features]time.Time),
	}

	// Internally this calls licensing.ValidateLicense, so if the license is
	// terminated, this will return an error (and Nomad should exit), but in
	// practice we've already validated the license when we called
	// LicenseConfig.Validate near the top of the NewServer setup.
	validator, err := cfg.validator()
	if err != nil {
		return nil, err
	}

	opts := &licensing.WatcherOptions{
		InitLicense: blob,
		Validator:   validator,
	}

	// Create the new watcher with options. Internally this calls
	// licensing.SetLicense, so if the license is terminated, this will return
	// an error (and Nomad should exit), but in practice we've already validated
	// the license above.
	watcher, _, err := licensing.NewWatcher(opts)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize nomad license: %w", err)
	}
	lw.watcher = watcher

	// TODO: NewWatcher calls licensing.SetLicense which itself calls
	// licensing.ValidateLicense already. Refactor so that we're only calling
	// the Nomad-specific storage methods here and not redoing the work the
	// library is already doing.
	err = lw.SetLicense(blob)
	if err != nil {
		return nil, fmt.Errorf("failed to set nomad license: %w", err)
	}
	return lw, nil
}

// Reload updates the license from the config
func (lw *LicenseWatcher) Reload(cfg *LicenseConfig) error {
	blob, err := cfg.licenseString()
	if err != nil {
		return err
	}
	if blob == "" {
		return nil
	}

	return lw.SetLicense(blob)
}

// License atomically returns the license watchers stored license
func (lw *LicenseWatcher) License() *nomadLicense.License {
	return lw.licenseInfo.Load().(*ServerLicense).license
}

func (lw *LicenseWatcher) LicenseBlob() string {
	return lw.licenseInfo.Load().(*ServerLicense).blob
}

// FileLicense returns the watchers file license that was used to initialize
// the server. It is not necessarily the license that the server is currently using
// if a newer license was added via raft or manual API invocation
func (lw *LicenseWatcher) FileLicense() string {
	return lw.fileLicense
}

// ValidateLicense validates that the given blob is a valid go-licensing
// license as well as a valid nomad license
func (lw *LicenseWatcher) ValidateLicense(blob string) (*nomadLicense.License, error) {
	lic, err := lw.watcher.ValidateLicense(blob)
	if err != nil {
		return nil, err
	}
	nLic, err := nomadLicense.NewLicense(lic)
	if err != nil {
		return nil, err
	}
	return nLic, nil
}

func (lw *LicenseWatcher) Features() nomadLicense.Features {
	lic := lw.License()
	if lic == nil {
		return nomadLicense.FeatureNone
	}

	// check if our license is still valid
	if _, err := lw.ValidateLicense(lw.FileLicense()); err != nil {
		return nomadLicense.FeatureNone
	}

	return lic.Features
}

// FeatureCheck determines if the given feature is included in License
// if emitLog is true, a log will only be sent once ever 5 minutes per feature
func (lw *LicenseWatcher) FeatureCheck(feature nomadLicense.Features, emitLog bool) error {
	if lw.hasFeature(feature) {
		return nil
	}

	err := fmt.Errorf("Feature %q is unlicensed", feature.String())

	if emitLog {
		// Only send log messages for a missing feature every 5 minutes
		lw.logMu.Lock()
		defer lw.logMu.Unlock()
		lastTime := lw.logTimes[feature]
		now := time.Now()
		if now.Sub(lastTime) > 5*time.Minute {
			lw.logger.Warn(err.Error())
			lw.logTimes[feature] = now
		}
	}

	return err
}

// SetLicense sets the server's license
func (lw *LicenseWatcher) SetLicense(blob string) error {
	blob = strings.TrimRight(blob, "\r\n")

	_, err := lw.watcher.ValidateLicense(blob)
	if err != nil {
		return fmt.Errorf("error validating license: %w", err)
	}

	if _, err := lw.watcher.SetLicense(blob); err != nil {
		lw.logger.Error("failed to persist license", "error", err)
		return err
	}

	startUpLicense, err := lw.watcher.License()
	if err != nil {
		return fmt.Errorf("failed to retrieve license: %w", err)
	}

	license, err := nomadLicense.NewLicense(startUpLicense)
	if err != nil {
		return fmt.Errorf("failed to convert license: %w", err)
	}

	// Store the expanded license and the corresponding blob
	lw.licenseInfo.Store(&ServerLicense{
		license: license,
		blob:    blob,
	})

	return nil
}

func (lw *LicenseWatcher) hasFeature(feature nomadLicense.Features) bool {
	return lw.Features().HasFeature(feature)
}

// start the license watching process in a goroutine. Callers are responsible
// for ensuring it is shut down properly
func (lw *LicenseWatcher) start(ctx context.Context) {
	go lw.monitorWatcher(ctx)
}

// monitorWatcher monitors the LicenseWatchers go-licensing watcher channels
//
// Nomad uses the go licensing watcher channels mostly to log.  Since Nomad
// does not shut down when a valid license has expired the ErrorCh logs.
func (lw *LicenseWatcher) monitorWatcher(ctx context.Context) {

	// Set up a ticker that allows us to emit a metric regarding the license
	// expiration.
	metricsTicker := time.NewTicker(time.Second)
	defer metricsTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			lw.watcher.Stop()
			return
		// Handle updated license from the watcher
		case <-lw.watcher.UpdateCh():
			// Check if server is shutting down
			select {
			case <-ctx.Done():
				return
			default:
			}
			lw.logger.Debug("received update from license manager")

		// Handle licensing watcher errors, primarily expirations or
		// terminations. Note that we don't exit on error or send the error
		// elsewhere, so even a terminated license won't stop the server. But a
		// terminated (not expired!) license will stop the server if it's
		// restarted.
		case err := <-lw.watcher.ErrorCh():
			lw.logger.Error("license expired, please update license", "error", err)

		case warnLicense := <-lw.watcher.WarningCh():
			lw.logger.Warn("license expiring", "time_left", time.Until(warnLicense.ExpirationTime).Truncate(time.Second))

		case <-metricsTicker.C:
			metrics.SetGauge([]string{"license", "expiration_time_epoch"}, float32(lw.License().ExpirationTime.Unix()))
		}
	}
}