// Command p2psync is the PairToPairSync daemon and its command line.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/aurelienbaraquin/pairtopairsync/internal/app"
	"github.com/aurelienbaraquin/pairtopairsync/internal/config"
	"github.com/aurelienbaraquin/pairtopairsync/internal/fsutil"
	"github.com/aurelienbaraquin/pairtopairsync/internal/identity"
	"github.com/aurelienbaraquin/pairtopairsync/internal/pairing"
)

const version = "0.1.0"

const usage = `p2psync - real-time folder sync with optional temporary relay points

Usage:
  p2psync [--home DIR] <command>

Setup:
  init [--name NAME] [--port N] [--api ADDR]
                                        create this device's identity and config
  id                                    print this device's ID (give it to your other devices)
  device add <ID> [--name N] [--addr HOST:PORT]...   (ID may also be a full /ip4/…/p2p/ID address)
  device list | device remove <ID>
  folder add <PATH> [--id ID] [--device ID[:relay]]... [--secret-file FILE|-]
                                        synchronise PATH (a normal directory) with those devices
  folder add-relay <STORAGE_PATH> --id ID --device ID... [--quota-mb N] [--retention-hours N]
                                        turn THIS device into a temporary relay for folder ID
  folder share <FOLDER_ID> <DEVICE_ID> [--relay]
  folder list

Pairing with a QR code (no IDs to copy, the folder secret is handed over securely):
  invite [--folder ID] [--as-relay] [--png FILE]
                                        show a single-use QR code (valid 10 min) for a new device
  pair <CODE> [--path DIR] [--quota-mb N]
                                        use the code shown by 'invite' (scan it with a phone, then paste its text)

Network (one admin introduces a new device to the others):
  admin enable | disable | promote <ID> | demote <ID>
  invite --network [--folders a,b] [--as-relay] [--admin] [--no-approve]
  pair <CODE> --dir DIR [--yes]         join through the admin that made the code
  requests | accept <ID> | reject <ID>  answer devices asking to join
  network                               who is who

Running:
  run [--log FILE]                      run the sync daemon in the foreground (--log: write the log to FILE)
  status                                show what the running daemon is doing
  ui                                    print the link of the local web page (status, QR invitations)
  version

The daemon reloads its configuration automatically when config.json changes.
`

func main() {
	home, rest, err := parseGlobal(os.Args[1:])
	if err != nil {
		fatal(err)
	}
	if len(rest) == 0 {
		fmt.Print(usage)
		return
	}
	cmd, args := rest[0], rest[1:]
	switch cmd {
	case "init":
		fs := flag.NewFlagSet("init", flag.ExitOnError)
		var name, apiAddr string
		var port int
		fs.StringVar(&name, "name", "", "device name")
		fs.IntVar(&port, "port", 0, "UDP+TCP port to listen on (default 22100)")
		fs.StringVar(&apiAddr, "api", "", "local status page address (default 127.0.0.1:8484, - to disable)")
		parse(fs, args)
		id, err := app.Init(home, name, port, apiAddr)
		check(err)
		fmt.Printf("Device ID: %s\nConfig:    %s\n", identity.Format(id), filepath.Join(home, app.ConfigFile))
	case "id":
		id, err := identity.LoadOrCreate(home)
		check(err)
		fmt.Println(identity.Format(id.ID))
	case "device":
		cmdDevice(home, args)
	case "folder":
		cmdFolder(home, args)
	case "ui":
		cmdUI(home)
	case "admin":
		cmdAdmin(home, args)
	case "network":
		cmdNetwork(home)
	case "requests":
		cmdRequests(home)
	case "accept", "reject":
		if len(args) != 1 {
			fatal(fmt.Errorf("usage: %s <REQUEST>", cmd))
		}
		cmdDecide(home, args[0], cmd == "accept")
	case "invite":
		cmdInvite(home, args)
	case "pair":
		cmdPair(home, args)
	case "run":
		cmdRun(home, args)
	case "status":
		check(status(home))
	case "version":
		fmt.Println("p2psync", version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
}

func parseGlobal(args []string) (home string, rest []string, err error) {
	home, err = app.DefaultHome()
	if err != nil {
		return
	}
	for len(args) > 0 {
		switch {
		case args[0] == "--home" && len(args) > 1:
			home, args = args[1], args[2:]
		case strings.HasPrefix(args[0], "--home="):
			home, args = strings.TrimPrefix(args[0], "--home="), args[1:]
		default:
			return home, args, nil
		}
	}
	return
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}

func check(err error) {
	if err != nil {
		fatal(err)
	}
}

type list []string

func (l *list) String() string     { return strings.Join(*l, ",") }
func (l *list) Set(v string) error { *l = append(*l, v); return nil }

// parse lets flags appear before or after positional arguments.
func parse(fs *flag.FlagSet, args []string) []string {
	var pos []string
	for {
		_ = fs.Parse(args) // ExitOnError
		args = fs.Args()
		if len(args) == 0 {
			return pos
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

func loadForEdit(home string) *config.Config {
	if _, err := os.Stat(filepath.Join(home, app.ConfigFile)); err != nil {
		fatal(fmt.Errorf("not initialised: run `p2psync init` first"))
	}
	cfg, err := app.LoadConfig(home)
	check(err)
	return cfg
}

func cmdDevice(home string, args []string) {
	if len(args) == 0 {
		fatal(fmt.Errorf("usage: device add|list|remove"))
	}
	switch args[0] {
	case "add":
		fs := flag.NewFlagSet("device add", flag.ExitOnError)
		var name string
		var addrs list
		fs.StringVar(&name, "name", "", "friendly name")
		fs.Var(&addrs, "addr", "host:port (repeatable); omit to rely on LAN discovery")
		pos := parse(fs, args[1:])
		if len(pos) != 1 {
			fatal(fmt.Errorf("usage: device add <ID> [--name N] [--addr HOST:PORT]"))
		}
		cfg := loadForEdit(home)
		check(app.AddDevice(cfg, pos[0], name, addrs))
		check(app.SaveConfig(home, cfg))
		fmt.Println("device added")
	case "list":
		cfg := loadForEdit(home)
		for _, d := range cfg.Devices {
			fmt.Printf("%s  %-16s %s\n", identity.Format(d.ID), d.Name, strings.Join(d.Addresses, ","))
		}
	case "remove":
		if len(args) != 2 {
			fatal(fmt.Errorf("usage: device remove <ID>"))
		}
		id, err := identity.Normalize(args[1])
		check(err)
		cfg := loadForEdit(home)
		if cfg.Device(id) == nil {
			fatal(fmt.Errorf("no such device: %s", identity.Format(id)))
		}
		var keep []config.Device
		for _, d := range cfg.Devices {
			if d.ID != id {
				keep = append(keep, d)
			}
		}
		cfg.Devices = keep
		for i := range cfg.Folders {
			var fd []config.FolderDevice
			for _, d := range cfg.Folders[i].Devices {
				if d.ID != id {
					fd = append(fd, d)
				}
			}
			cfg.Folders[i].Devices = fd
		}
		check(app.SaveConfig(home, cfg))
		fmt.Println("device removed from every folder (a running daemon applies this by itself)")
	default:
		fatal(fmt.Errorf("unknown device command %q", args[0]))
	}
}

func cmdFolder(home string, args []string) {
	if len(args) == 0 {
		fatal(fmt.Errorf("usage: folder add|add-relay|share|list"))
	}
	switch args[0] {
	case "add", "add-relay":
		isRelay := args[0] == "add-relay"
		fs := flag.NewFlagSet(args[0], flag.ExitOnError)
		var id, secret, secretFile string
		var devs list
		var quota int64
		var retention int
		fs.StringVar(&id, "id", "", "folder ID shared by all devices (default: directory name)")
		fs.Var(&devs, "device", "device ID to share with; suffix :relay if that device is a temporary relay (repeatable)")
		fs.StringVar(&secret, "secret", "", "full folder: secret shared by all your full devices (generated if omitted); visible in the process list, prefer --secret-file")
		fs.StringVar(&secretFile, "secret-file", "", "full folder: read the secret from this file, or from standard input if -")
		fs.Int64Var(&quota, "quota-mb", 0, "relay: maximum temporary storage in MB (default 2048)")
		fs.IntVar(&retention, "retention-hours", 0, "relay: drop undelivered changes after this many hours (default 720)")
		pos := parse(fs, args[1:])
		if len(pos) != 1 {
			fatal(fmt.Errorf("usage: folder %s <PATH> [--id ID] [--device ID[:relay]]", args[0]))
		}
		role := config.RoleFull
		if isRelay {
			role = config.RoleRelay
			if id == "" {
				fatal(fmt.Errorf("a relay needs --id with the ID of the folder it relays"))
			}
		}
		if secretFile != "" {
			if secret != "" {
				fatal(fmt.Errorf("use either --secret or --secret-file"))
			}
			var err error
			secret, err = readSecret(secretFile)
			check(err)
		}
		cfg := loadForEdit(home)
		sec, err := app.AddFolder(cfg, id, pos[0], role, quota, retention, secret)
		check(err)
		fid := id
		if fid == "" {
			fid = config.SlugID(filepath.Base(pos[0]))
		}
		for _, d := range devs {
			dev, isRel := strings.CutSuffix(d, ":relay")
			check(app.ShareFolder(cfg, fid, dev, isRel))
		}
		check(app.SaveConfig(home, cfg))
		fmt.Printf("folder %q added (%s)\n", fid, role)
		if sec != "" && secret == "" {
			fmt.Printf("\nFolder secret (give it ONLY to your other full devices (--secret-file -); never to a relay):\n  %s\n", sec)
		}
	case "share":
		fs := flag.NewFlagSet("folder share", flag.ExitOnError)
		rel := fs.Bool("relay", false, "the device is a temporary relay")
		pos := parse(fs, args[1:])
		if len(pos) != 2 {
			fatal(fmt.Errorf("usage: folder share <FOLDER_ID> <DEVICE_ID> [--relay]"))
		}
		cfg := loadForEdit(home)
		check(app.ShareFolder(cfg, pos[0], pos[1], *rel))
		check(app.SaveConfig(home, cfg))
		fmt.Println("shared")
	case "list":
		cfg := loadForEdit(home)
		for _, f := range cfg.Folders {
			fmt.Printf("%-16s %-6s %s\n", f.ID, f.Role, f.Path)
			for _, d := range f.Devices {
				kind := "full"
				if d.Relay {
					kind = "relay"
				}
				name := d.ID
				if dd := cfg.Device(d.ID); dd != nil && dd.Name != "" {
					name = dd.Name
				}
				fmt.Printf("    with %-16s (%s)\n", name, kind)
			}
		}
	default:
		fatal(fmt.Errorf("unknown folder command %q", args[0]))
	}
}

// cmdRun parses the run options and runs the daemon until it is told to stop.
func cmdRun(home string, args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	var logFile string
	fs.StringVar(&logFile, "log", "", "write the log to this file instead of the terminal")
	parse(fs, args)

	var w io.Writer = os.Stderr
	if logFile != "" {
		f, err := openLog(logFile)
		check(err)
		defer f.Close()
		w = f
	}
	logger := slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := run(home, logger); err != nil {
		logger.Error("stopped", "err", err)
		fatal(err)
	}
}

// openLog opens a log file for appending, keeping one previous generation when it
// grows past a few megabytes (a daemon without a console has nothing else to show).
func openLog(path string) (*os.File, error) {
	const maxLog = 5 << 20
	if fi, err := os.Stat(path); err == nil && fi.Size() > maxLog {
		_ = fsutil.Remove(path + ".1")
		_ = fsutil.Rename(path, path+".1")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
}

// loadConfig reads config.json, trying a few times: an editor may be half-way
// through saving it.
func loadConfig(home string) (cfg *config.Config, err error) {
	for i := 0; i < 3; i++ {
		if cfg, err = app.LoadConfig(home); err == nil {
			return cfg, nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	return nil, err
}

// run keeps a device running, restarting it when its configuration changes. It
// only gives up when told to: a configuration that cannot be read or started
// (a typo, a port still in use after a crash) is logged and the previous working
// configuration keeps running, or the start is retried with a growing delay.
func run(home string, logger *slog.Logger) error {
	id, err := identity.LoadOrCreate(home)
	if err != nil {
		return err
	}
	cfgPath := filepath.Join(home, app.ConfigFile)
	if _, err := os.Stat(cfgPath); err != nil {
		return fmt.Errorf("not initialised: run `p2psync init` first")
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	reload := make(chan struct{}, 1)
	notifyReload(reload)

	var (
		a     *app.App
		good  *config.Config // last configuration that started
		delay = time.Second
	)
	start := func(cfg *config.Config) error {
		next, err := app.Start(context.Background(), home, cfg, id, logger)
		if err != nil {
			return err
		}
		a, good = next, cfg
		logger.Info("running", "device", identity.Format(id.ID), "port", cfg.ListenPort)
		return nil
	}

	for {
		// Note the version of config.json *before* loading it: a change made in between
		// must trigger the next restart, not be mistaken for the version we already run.
		stamp := modTime(cfgPath)
		cfg, err := loadConfig(home)
		switch {
		case err != nil && a != nil:
			logger.Error("config.json is not usable, keeping the previous configuration", "err", err)
		case err != nil:
			logger.Error("config.json is not usable, waiting for a fix", "err", err)
		default:
			if a != nil {
				logger.Info("configuration changed, restarting")
				a.Stop()
				a = nil
			}
			if err := start(cfg); err != nil {
				logger.Error("cannot start with this configuration", "err", err)
				if good != nil && good != cfg {
					if err := start(good); err != nil {
						logger.Error("cannot restart the previous configuration either", "err", err)
					} else {
						logger.Warn("running the previous configuration; fix config.json to apply the new one")
					}
				}
			} else {
				delay = time.Second
			}
		}

		poll := time.NewTicker(3 * time.Second)
		for restart := false; !restart; {
			var retry <-chan time.Time
			if a == nil {
				retry = time.After(delay)
			}
			select {
			case <-sig:
				poll.Stop()
				logger.Info("shutting down")
				if a == nil {
					return nil
				}
				stopped := make(chan struct{})
				go func() { a.Stop(); close(stopped) }()
				select {
				case <-stopped:
				case <-sig: // second Ctrl-C
				case <-time.After(15 * time.Second):
					logger.Warn("shutdown timed out, exiting anyway")
				}
				return nil
			case <-reload:
				restart = true
			case <-poll.C:
				restart = modTime(cfgPath) != stamp
			case <-retry:
				restart = true
				delay = min(2*delay, 30*time.Second)
			}
		}
		poll.Stop()
	}
}

func modTime(p string) time.Time {
	fi, err := os.Stat(p)
	if err != nil {
		return time.Time{}
	}
	return fi.ModTime()
}

func status(home string) error {
	cfg, err := app.LoadConfig(home)
	if err != nil {
		return err
	}
	addr := cfg.APIAddr
	if addr == "" || addr == "-" {
		return fmt.Errorf("status API is disabled in the configuration")
	}
	resp, err := apiGet(home, addr)
	if err != nil {
		return fmt.Errorf("daemon not reachable at %s (is `p2psync run` started?)", addr)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var st struct {
		DeviceID   string           `json:"device_id"`
		DeviceName string           `json:"device_name"`
		Addresses  []string         `json:"addresses"`
		Folders    []map[string]any `json:"folders"`
		Peers      []struct {
			Name    string `json:"name"`
			Relayed bool   `json:"relayed"`
		} `json:"peers"`
	}
	if err := json.Unmarshal(body, &st); err != nil {
		return err
	}
	fmt.Printf("%s (%s)\n", st.DeviceName, st.DeviceID)
	for _, f := range st.Folders {
		if f["role"] == "relay" {
			fmt.Printf("  [relay] %-14s %v pending change(s), %.1f MB stored / %.0f MB quota\n",
				f["id"], f["pending_changes"], num(f["stored_bytes"])/1e6, num(f["quota_bytes"])/1e6)
		} else {
			fmt.Printf("  [sync]  %-14s %-9v %v files, %v to sync   %v\n", f["id"], f["state"], f["local_files"], f["need_files"], f["path"])
		}
		if e, _ := f["error"].(string); e != "" {
			fmt.Println("          ERROR:", e)
		}
	}
	for _, p := range st.Peers {
		path := "direct"
		if p.Relayed {
			path = "via one of your devices (relayed)"
		}
		fmt.Printf("  connected: %-16s %s\n", p.Name, path)
	}
	if len(st.Peers) == 0 {
		fmt.Println("  no device connected")
	}
	if len(st.Addresses) > 0 {
		fmt.Println("\nTo pair another device with this one, give it one of these addresses:")
		for _, a := range st.Addresses {
			fmt.Println("  ", a)
		}
	}
	return nil
}

func num(v any) float64 { f, _ := v.(float64); return f }

// ---- QR pairing ---------------------------------------------------------------------

// daemonStatus fetches the running daemon's status document.
func daemonStatus(home string, cfg *config.Config) (id string, addrs []string, err error) {
	if cfg.APIAddr == "" || cfg.APIAddr == "-" {
		return "", nil, fmt.Errorf("the status API is disabled in the configuration; it is needed to learn this device's addresses")
	}
	resp, err := apiGet(home, cfg.APIAddr)
	if err != nil {
		return "", nil, pairing.ErrNoDaemon
	}
	defer resp.Body.Close()
	var st struct {
		DeviceID  string   `json:"device_id"`
		Addresses []string `json:"addresses"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return "", nil, err
	}
	return st.DeviceID, st.Addresses, nil
}

func cmdInvite(home string, args []string) {
	fs := flag.NewFlagSet("invite", flag.ExitOnError)
	var folder, png, folders string
	var asRelay, network, admin, noApprove bool
	var ttl time.Duration
	fs.BoolVar(&network, "network", false, "invite into the whole network through this admin (not just one folder)")
	fs.StringVar(&folders, "folders", "", "with --network: folders to share (default: all)")
	fs.BoolVar(&admin, "admin", false, "with --network: the new device becomes an admin too")
	fs.BoolVar(&noApprove, "no-approve", false, "with --network: do not ask for confirmation when the device connects")
	fs.StringVar(&folder, "folder", "", "folder to invite the device to (default: the only folder)")
	fs.BoolVar(&asRelay, "as-relay", false, "the new device will be a temporary relay (e.g. a phone), not a full copy")
	fs.StringVar(&png, "png", "", "also write the QR code to this PNG file")
	fs.DurationVar(&ttl, "ttl", pairing.DefaultTTL, "how long the invitation stays valid")
	parse(fs, args)

	cfg := loadForEdit(home)
	if network {
		cmdNetworkInvite(home, cfg, folders, asRelay, admin, noApprove, png, ttl)
		return
	}
	if folder == "" {
		if len(cfg.Folders) != 1 {
			fatal(fmt.Errorf("several folders exist: choose one with --folder"))
		}
		folder = cfg.Folders[0].ID
	}
	selfID, addrs, err := daemonStatus(home, cfg)
	check(err)
	inv, err := app.CreateInvite(home, cfg, selfID, addrs, folder, asRelay, ttl)
	check(err)
	code := inv.Encode()
	qr, err := pairing.QRText(code)
	check(err)
	fmt.Println(qr)
	if png != "" {
		b, err := pairing.QRPNG(code, 512)
		check(err)
		check(os.WriteFile(png, b, 0o600))
		fmt.Println("QR code saved to", png)
	}
	role := "a full copy"
	if inv.InviteeRelay {
		role = "a temporary relay"
	}
	if inv.InviterRelay {
		role = "a full copy (this device is the relay)"
	}
	fmt.Printf("Invitation to folder %q as %s. Valid until %s, usable once.\n", inv.Folder, role, time.Unix(inv.Exp, 0).Format("15:04:05"))
	fmt.Println("Scan the QR code with a phone and paste the text it shows into `p2psync pair`, or give this text directly:")
	fmt.Println()
	fmt.Println(code)
	fmt.Println()
	fmt.Println("Treat it like a password: anyone who has it before it expires can join this folder.")
}

func cmdPair(home string, args []string) {
	fs := flag.NewFlagSet("pair", flag.ExitOnError)
	var path, dir string
	var quota int64
	var yes bool
	fs.StringVar(&dir, "dir", "", "network invitation: directory under which the granted folders are created")
	fs.BoolVar(&yes, "yes", false, "network invitation: do not ask for confirmation")
	fs.StringVar(&path, "path", "", "where to put the folder (or the relay's temporary storage) on this device")
	fs.Int64Var(&quota, "quota-mb", 0, "relay: maximum temporary storage in MB")
	pos := parse(fs, args)
	if len(pos) != 1 {
		fatal(fmt.Errorf("usage: pair <CODE> [--path DIR]"))
	}
	inv, err := pairing.Decode(pos[0])
	check(err)
	if inv.Expired(time.Now()) {
		fatal(fmt.Errorf("this invitation has expired; ask for a new one"))
	}
	if inv.Net {
		cmdPairNetwork(home, inv, dir, yes)
		return
	}

	// Create the identity and a default config on first use.
	_, err = app.Init(home, "", 0, "")
	check(err)
	id, err := identity.LoadOrCreate(home)
	check(err)
	cfg, err := app.LoadConfig(home)
	check(err)
	opts := app.JoinOptions{Path: path, QuotaMB: quota}
	if opts.Path == "" && inv.InviteeRelay {
		opts.Path = filepath.Join(home, "relay", inv.Folder)
	}
	check(app.PreflightJoin(cfg, inv, opts)) // before the invitation is spent
	check(app.CheckJoinPath(home, cfg, inv, opts))

	fmt.Printf("Connecting to %s…\n", firstNonEmpty(inv.Name, identity.Format(inv.ID)))
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	resp, err := pairing.Dial(ctx, id.Key, inv, app.GuestRequest(cfg))
	check(err)

	// The invitation is spent now: if this device cannot record the result, say what
	// it needs to be added by hand rather than losing it.
	err = app.Join(cfg, inv, resp, opts)
	if err == nil {
		err = app.SaveConfig(home, cfg)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "The other device accepted us, but this device could not save the result: %v\n", err)
		fmt.Fprintf(os.Stderr, "Add it by hand:\n  p2psync device add %s --name %q\n", identity.Format(inv.ID), firstNonEmpty(resp.Name, inv.Name))
		if resp.Secret != "" {
			fmt.Fprintf(os.Stderr, "  echo %s | p2psync folder add <PATH> --id %s --device %s --secret-file -\n", resp.Secret, inv.Folder, identity.Format(inv.ID))
		}
		os.Exit(1)
	}
	switch {
	case inv.InviteeRelay:
		fmt.Printf("Paired. This device is now a temporary relay for %q (storage: %s).\n", inv.Folder, opts.Path)
	case inv.InviterRelay:
		fmt.Printf("Paired. %s is now a temporary relay for your folder %q.\n", firstNonEmpty(resp.Name, "The other device"), inv.Folder)
	default:
		fmt.Printf("Paired with %s. Folder %q will sync into %s.\n", firstNonEmpty(resp.Name, "the other device"), inv.Folder, cfg.Folder(inv.Folder).Path)
	}
	fmt.Println("If the daemon is running it reloads by itself; otherwise start it with `p2psync run`.")
}

// readSecret reads a folder secret from a file, or from standard input for "-".
func readSecret(path string) (string, error) {
	var b []byte
	var err error
	if path == "-" {
		b, err = io.ReadAll(io.LimitReader(os.Stdin, 4096))
	} else {
		b, err = os.ReadFile(path)
	}
	if err != nil {
		return "", err
	}
	sec := strings.TrimSpace(string(b))
	if sec == "" {
		return "", fmt.Errorf("the secret is empty")
	}
	return sec, nil
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

var apiClient = &http.Client{Timeout: 10 * time.Second}

// apiGet queries the daemon's status endpoint with the local access token.
func apiGet(home, addr string) (*http.Response, error) {
	tok, err := app.APIToken(home)
	if err != nil {
		return nil, err
	}
	req, _ := http.NewRequest("GET", "http://"+addr+"/api/status", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := apiClient.Do(req)
	if err == nil && resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("daemon answered %s", resp.Status)
	}
	return resp, err
}

func cmdUI(home string) {
	cfg := loadForEdit(home)
	tok, err := app.APIToken(home)
	check(err)
	if cfg.APIAddr == "" || cfg.APIAddr == "-" {
		fatal(fmt.Errorf("the status page is disabled in the configuration"))
	}
	fmt.Printf("Open this link in your browser (it contains your local access token):\n  http://%s/#t=%s\n", cfg.APIAddr, tok)
}
