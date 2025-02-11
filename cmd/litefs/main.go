// go:build linux
package main

import (
	"context"
	"expvar"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mattn/go-shellwords"
	"github.com/superfly/litefs"
	"github.com/superfly/litefs/consul"
	"github.com/superfly/litefs/fuse"
	"github.com/superfly/litefs/http"
	"gopkg.in/yaml.v3"
)

// Build information.
var (
	Version = ""
	Commit  = ""
)

func main() {
	log.SetFlags(0)

	signalCh := make(chan os.Signal, 2)
	signal.Notify(signalCh, syscall.SIGINT, syscall.SIGTERM)

	ctx, cancel := context.WithCancel(context.Background())

	// Set HOSTNAME environment variable, if unset by environment.
	// This can be used for variable expansion in the config file.
	if os.Getenv("HOSTNAME") == "" {
		hostname, _ := os.Hostname()
		_ = os.Setenv("HOSTNAME", hostname)
	}

	// Initialize binary and parse CLI flags & config.
	m := NewMain()
	if err := m.ParseFlags(ctx, os.Args[1:]); err == flag.ErrHelp {
		os.Exit(2)
	} else if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %s\n", err)
		os.Exit(2)
	}

	// Validate configuration.
	if err := m.Validate(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %s\n", err)
		os.Exit(2)
	}

	if err := m.Run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %s\n", err)

		// Only exit the process if enabled in the config. A user want to
		// continue running so that an ephemeral node can be debugged intsead
		// of continually restarting on error.
		if m.Config.ExitOnError {
			_ = m.Close()
			os.Exit(1)
		}
	}

	fmt.Println("waiting for signal or subprocess to exit")

	// Wait for signal or subcommand exit to stop program.
	select {
	case <-m.execCh:
		cancel()
		fmt.Println("subprocess exited, litefs shutting down")

	case sig := <-signalCh:
		if m.cmd != nil {
			fmt.Println("sending signal to exec process")
			if err := m.cmd.Process.Signal(sig); err != nil {
				fmt.Fprintln(os.Stderr, "cannot signal exec process:", err)
				os.Exit(1)
			}

			fmt.Println("waiting for exec process to close")
			if err := <-m.execCh; err != nil && !strings.HasPrefix(err.Error(), "signal:") {
				fmt.Fprintln(os.Stderr, "cannot wait for exec process:", err)
				os.Exit(1)
			}
		}

		cancel()
		fmt.Println("signal received, litefs shutting down")
	}

	if err := m.Close(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	fmt.Println("litefs shut down complete")
}

// Main represents the command line program.
type Main struct {
	cmd    *exec.Cmd  // subcommand
	execCh chan error // subcommand error channel

	Config Config

	Store      *litefs.Store
	Leaser     litefs.Leaser
	FileSystem *fuse.FileSystem
	HTTPServer *http.Server

	// Used for generating the advertise URL for testing.
	AdvertiseURLFn func() string
}

// NewMain returns a new instance of Main.
func NewMain() *Main {
	return &Main{
		execCh: make(chan error),
		Config: NewConfig(),
	}
}

// ParseFlags parses the command line flags & config file.
func (m *Main) ParseFlags(ctx context.Context, args []string) (err error) {
	// Split the args list if there is a double dash arg included. Arguments
	// after the double dash are used as the "exec" subprocess config option.
	args0, args1 := splitArgs(args)

	fs := flag.NewFlagSet("litefs", flag.ContinueOnError)
	configPath := fs.String("config", "", "config file path")
	noExpandEnv := fs.Bool("no-expand-env", false, "do not expand env vars in config")
	if err := fs.Parse(args0); err != nil {
		return err
	} else if fs.NArg() > 0 {
		return fmt.Errorf("too many arguments, specify a '--' to specify an exec command")
	}

	if err := m.parseConfig(ctx, *configPath, !*noExpandEnv); err != nil {
		return err
	}

	// Override "exec" field if specified on the CLI.
	if args1 != nil {
		m.Config.Exec = strings.Join(args1, " ")
	}

	return nil
}

// parseConfig parses the configuration file from configPath, if specified.
// Otherwise searches the standard list of search paths. Returns an error if
// no configuration files could be found.
func (m *Main) parseConfig(ctx context.Context, configPath string, expandEnv bool) (err error) {
	// Only read from explicit path, if specified. Report any error.
	if configPath != "" {
		return ReadConfigFile(&m.Config, configPath, expandEnv)
	}

	// Otherwise attempt to read each config path until we succeed.
	for _, path := range configSearchPaths() {
		if path, err = filepath.Abs(path); err != nil {
			return err
		}

		if err := ReadConfigFile(&m.Config, path, expandEnv); err == nil {
			fmt.Printf("config file read from %s\n", path)
			return nil
		} else if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("cannot read config file at %s: %s", path, err)
		}
	}
	return fmt.Errorf("config file not found")
}

// Validate validates the application's configuration.
func (m *Main) Validate(ctx context.Context) (err error) {
	if m.Config.MountDir == "" {
		return fmt.Errorf("mount directory required")
	} else if m.Config.DataDir == "" {
		return fmt.Errorf("data directory required")
	} else if m.Config.MountDir == m.Config.DataDir {
		return fmt.Errorf("mount directory and data directory cannot be the same path")
	}

	// Enforce exactly one lease mode.
	if m.Config.Consul != nil && m.Config.Static != nil {
		return fmt.Errorf("cannot specify both 'consul' and 'static' lease modes")
	} else if m.Config.Consul == nil && m.Config.Static == nil {
		return fmt.Errorf("must specify a lease mode ('consul', 'static')")
	}

	return nil
}

// configSearchPaths returns paths to search for the config file. It starts with
// the current directory, then home directory, if available. And finally it tries
// to read from the /etc directory.
func configSearchPaths() []string {
	a := []string{"litefs.yml"}
	if u, _ := user.Current(); u != nil && u.HomeDir != "" {
		a = append(a, filepath.Join(u.HomeDir, "litefs.yml"))
	}
	a = append(a, "/etc/litefs.yml")
	return a
}

func (m *Main) Close() (err error) {
	if m.HTTPServer != nil {
		if e := m.HTTPServer.Close(); err == nil {
			err = e
		}
	}

	if m.FileSystem != nil {
		if e := m.FileSystem.Unmount(); err == nil {
			err = e
		}
	}

	if m.Store != nil {
		if e := m.Store.Close(); err == nil {
			err = e
		}
	}

	return err
}

func (m *Main) Run(ctx context.Context) (err error) {
	// Print version & commit information, if available.
	if Version != "" {
		log.Printf("LiteFS %s, commit=%s", Version, Commit)
	} else if Commit != "" {
		log.Printf("LiteFS commit=%s", Commit)
	} else {
		log.Printf("LiteFS development build")
	}

	// Start listening on HTTP server first so we can determine the URL.
	if err := m.initStore(ctx); err != nil {
		return fmt.Errorf("cannot init store: %w", err)
	} else if err := m.initHTTPServer(ctx); err != nil {
		return fmt.Errorf("cannot init http server: %w", err)
	}

	// Instantiate leaser.
	if m.Config.Consul != nil {
		log.Println("Using Consul to determine primary")
		if err := m.initConsul(ctx); err != nil {
			return fmt.Errorf("cannot init consul: %w", err)
		}
	} else { // static
		log.Printf("Using static primary: is-primary=%v hostname=%s advertise-url=%s", m.Config.Static.Primary, m.Config.Static.Hostname, m.Config.Static.AdvertiseURL)
		m.Leaser = litefs.NewStaticLeaser(m.Config.Static.Primary, m.Config.Static.Hostname, m.Config.Static.AdvertiseURL)
	}

	if err := m.openStore(ctx); err != nil {
		return fmt.Errorf("cannot open store: %w", err)
	}

	if err := m.initFileSystem(ctx); err != nil {
		return fmt.Errorf("cannot init file system: %w", err)
	}
	log.Printf("LiteFS mounted to: %s", m.FileSystem.Path())

	m.HTTPServer.Serve()
	log.Printf("http server listening on: %s", m.HTTPServer.URL())

	// Wait until the store either becomes primary or connects to the primary.
	log.Printf("waiting to connect to cluster")
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-m.Store.ReadyCh():
		log.Printf("connected to cluster, ready")
	}

	// Execute subcommand, if specified in config.
	if err := m.execCmd(ctx); err != nil {
		return fmt.Errorf("cannot exec: %w", err)
	}

	return nil
}

func (m *Main) initConsul(ctx context.Context) (err error) {
	// TEMP: Allow non-localhost addresses.

	// Use hostname from OS, if not specified.
	hostname := m.Config.Consul.Hostname
	if hostname == "" {
		if hostname, err = os.Hostname(); err != nil {
			return err
		}
	}

	// Determine the advertise URL for the LiteFS API.
	// Default to use the hostname and HTTP port. Also allow injection for tests.
	advertiseURL := m.Config.Consul.AdvertiseURL
	if m.AdvertiseURLFn != nil {
		advertiseURL = m.AdvertiseURLFn()
	}
	if advertiseURL == "" && hostname != "" {
		advertiseURL = fmt.Sprintf("http://%s:%d", hostname, m.HTTPServer.Port())
	}

	leaser := consul.NewLeaser(m.Config.Consul.URL, hostname, advertiseURL)
	if v := m.Config.Consul.Key; v != "" {
		leaser.Key = v
	}
	if v := m.Config.Consul.TTL; v > 0 {
		leaser.TTL = v
	}
	if v := m.Config.Consul.LockDelay; v > 0 {
		leaser.LockDelay = v
	}
	if err := leaser.Open(); err != nil {
		return fmt.Errorf("cannot connect to consul: %w", err)
	}
	log.Printf("initializing consul: key=%s url=%s hostname=%s advertise-url=%s", m.Config.Consul.Key, m.Config.Consul.URL, hostname, advertiseURL)

	m.Leaser = leaser
	return nil
}

func (m *Main) initStore(ctx context.Context) error {
	m.Store = litefs.NewStore(m.Config.DataDir, m.Config.Candidate)
	m.Store.Debug = m.Config.Debug
	m.Store.StrictVerify = m.Config.StrictVerify
	m.Store.RetentionDuration = m.Config.Retention.Duration
	m.Store.RetentionMonitorInterval = m.Config.Retention.MonitorInterval
	m.Store.Client = http.NewClient()
	return nil
}

func (m *Main) openStore(ctx context.Context) error {
	m.Store.Leaser = m.Leaser
	if err := m.Store.Open(); err != nil {
		return err
	}

	// Register expvar variable once so it doesn't panic during tests.
	expvarOnce.Do(func() { expvar.Publish("store", (*litefs.StoreVar)(m.Store)) })

	return nil
}

func (m *Main) initFileSystem(ctx context.Context) error {
	// Build the file system to interact with the store.
	fsys := fuse.NewFileSystem(m.Config.MountDir, m.Store)
	if err := fsys.Mount(); err != nil {
		return fmt.Errorf("cannot open file system: %s", err)
	}

	// Attach file system to store so it can invalidate the page cache.
	m.Store.Invalidator = fsys

	m.FileSystem = fsys
	return nil
}

func (m *Main) initHTTPServer(ctx context.Context) error {
	server := http.NewServer(m.Store, m.Config.HTTP.Addr)
	if err := server.Listen(); err != nil {
		return fmt.Errorf("cannot open http server: %w", err)
	}
	m.HTTPServer = server
	return nil
}

func (m *Main) execCmd(ctx context.Context) error {
	// Exit if no subcommand specified.
	if m.Config.Exec == "" {
		return nil
	}

	// Execute subcommand process.
	args, err := shellwords.Parse(m.Config.Exec)
	if err != nil {
		return fmt.Errorf("cannot parse exec command: %w", err)
	}

	log.Printf("starting subprocess: %s %v", args[0], args[1:])

	m.cmd = exec.CommandContext(ctx, args[0], args[1:]...)
	m.cmd.Env = os.Environ()
	m.cmd.Stdout = os.Stdout
	m.cmd.Stderr = os.Stderr
	if err := m.cmd.Start(); err != nil {
		return fmt.Errorf("cannot start exec command: %w", err)
	}
	go func() { m.execCh <- m.cmd.Wait() }()

	return nil
}

var expvarOnce sync.Once

// NOTE: Update etc/litefs.yml configuration file after changing the structure below.

// Config represents a configuration for the binary process.
type Config struct {
	MountDir     string `yaml:"mount-dir"`
	DataDir      string `yaml:"data-dir"`
	Exec         string `yaml:"exec"`
	Candidate    bool   `yaml:"candidate"`
	Debug        bool   `yaml:"debug"`
	ExitOnError  bool   `yaml:"exit-on-error"`
	StrictVerify bool   `yaml:"-"`

	Retention RetentionConfig `yaml:"retention"`
	HTTP      HTTPConfig      `yaml:"http"`
	Consul    *ConsulConfig   `yaml:"consul"`
	Static    *StaticConfig   `yaml:"static"`
}

// NewConfig returns a new instance of Config with defaults set.
func NewConfig() Config {
	var config Config
	config.Candidate = true
	config.ExitOnError = true
	config.Retention.Duration = litefs.DefaultRetentionDuration
	config.Retention.MonitorInterval = litefs.DefaultRetentionMonitorInterval
	config.HTTP.Addr = http.DefaultAddr
	return config
}

// RetentionConfig represents the configuration for LTX file retention.
type RetentionConfig struct {
	Duration        time.Duration `yaml:"duration"`
	MonitorInterval time.Duration `yaml:"monitor-interval"`
}

// HTTPConfig represents the configuration for the HTTP server.
type HTTPConfig struct {
	Addr string `yaml:"addr"`
}

// ConsulConfig represents the configuration for a Consul leaser.
type ConsulConfig struct {
	URL          string        `yaml:"url"`
	Hostname     string        `yaml:"hostname"`
	AdvertiseURL string        `yaml:"advertise-url"`
	Key          string        `yaml:"key"`
	TTL          time.Duration `yaml:"ttl"`
	LockDelay    time.Duration `yaml:"lock-delay"`
}

// StaticConfig represents the configuration for a static leaser.
type StaticConfig struct {
	Primary      bool   `yaml:"primary"`
	Hostname     string `yaml:"hostname"`
	AdvertiseURL string `yaml:"advertise-url"`
}

// ReadConfigFile unmarshals config from filename. If expandEnv is true then
// environment variables are expanded in the config.
func ReadConfigFile(config *Config, filename string, expandEnv bool) error {
	// Read configuration.
	buf, err := os.ReadFile(filename)
	if err != nil {
		return err
	}

	// Expand environment variables, if enabled.
	if expandEnv {
		buf = []byte(ExpandEnv(string(buf)))
	}

	if err := yaml.Unmarshal(buf, &config); err != nil {
		return err
	}
	return nil
}

// ExpandEnv replaces environment variables just like os.ExpandEnv() but also
// allows for equality/inequality binary expressions within the ${} form.
func ExpandEnv(s string) string {
	return os.Expand(s, func(v string) string {
		v = strings.TrimSpace(v)

		if a := expandExprSingleQuote.FindStringSubmatch(v); a != nil {
			if a[2] == "==" {
				return strconv.FormatBool(os.Getenv(a[1]) == a[3])
			}
			return strconv.FormatBool(os.Getenv(a[1]) != a[3])
		}

		if a := expandExprDoubleQuote.FindStringSubmatch(v); a != nil {
			if a[2] == "==" {
				return strconv.FormatBool(os.Getenv(a[1]) == a[3])
			}
			return strconv.FormatBool(os.Getenv(a[1]) != a[3])
		}

		if a := expandExprVar.FindStringSubmatch(v); a != nil {
			if a[2] == "==" {
				return strconv.FormatBool(os.Getenv(a[1]) == os.Getenv(a[3]))
			}
			return strconv.FormatBool(os.Getenv(a[1]) != os.Getenv(a[3]))
		}

		return os.Getenv(v)
	})
}

var (
	expandExprSingleQuote = regexp.MustCompile(`^(\w+)\s*(==|!=)\s*'(.*)'$`)
	expandExprDoubleQuote = regexp.MustCompile(`^(\w+)\s*(==|!=)\s*"(.*)"$`)
	expandExprVar         = regexp.MustCompile(`^(\w+)\s*(==|!=)\s*(\w+)$`)
)

// splitArgs returns the list of args before and after a "--" arg. If the double
// dash is not specified, then args0 is args and args1 is empty.
func splitArgs(args []string) (args0, args1 []string) {
	for i, v := range args {
		if v == "--" {
			return args[:i], args[i+1:]
		}
	}
	return args, nil
}
