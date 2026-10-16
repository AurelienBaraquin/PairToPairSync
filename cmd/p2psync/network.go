package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aurelienbaraquin/pairtopairsync/internal/app"
	"github.com/aurelienbaraquin/pairtopairsync/internal/config"
	"github.com/aurelienbaraquin/pairtopairsync/internal/identity"
	"github.com/aurelienbaraquin/pairtopairsync/internal/pairing"
)

func cmdAdmin(home string, args []string) {
	if len(args) == 0 {
		fatal(fmt.Errorf("usage: admin enable|disable|promote <ID>|demote <ID>"))
	}
	switch args[0] {
	case "enable", "disable":
		cfg := loadForEdit(home)
		cfg.Admin = args[0] == "enable"
		check(app.SaveConfig(home, cfg))
		if cfg.Admin {
			fmt.Println("this device is now a network admin: it vouches for the devices it was paired with.")
			fmt.Println("Create invitations with `p2psync invite --network`.")
		} else {
			fmt.Println("this device is no longer an admin. Grants it already issued stay valid; revoke a device with `device remove`.")
		}
	case "promote", "demote":
		if len(args) != 2 {
			fatal(fmt.Errorf("usage: admin %s <ID>", args[0]))
		}
		id, err := identity.Normalize(args[1])
		check(err)
		cfg := loadForEdit(home)
		d := cfg.Device(id)
		if d == nil {
			fatal(fmt.Errorf("no such device: pair with it first"))
		}
		d.Admin = args[0] == "promote"
		check(app.SaveConfig(home, cfg))
		if d.Admin {
			fmt.Printf("%s is now trusted as a network admin on this device", orID(d.Name, d.ID))
			if cfg.Admin {
				fmt.Print(", and the other devices will be told to trust it too")
			}
			fmt.Println(".")
		} else {
			fmt.Printf("%s is no longer trusted as an admin on this device.\n", orID(d.Name, d.ID))
		}
	default:
		fatal(fmt.Errorf("unknown admin command %q", args[0]))
	}
}

func orID(name, id string) string {
	if name != "" {
		return name
	}
	return identity.Format(id)
}

// cmdNetworkInvite prints a QR code for a network invitation.
func cmdNetworkInvite(home string, cfg *config.Config, folders string, asRelay, admin, noApprove bool, png string, ttl time.Duration) {
	selfID, addrs, err := daemonStatus(home, cfg)
	check(err)
	var list []string
	for _, f := range strings.Split(folders, ",") {
		if f = strings.TrimSpace(f); f != "" {
			list = append(list, f)
		}
	}
	inv, err := app.CreateNetworkInvite(home, cfg, selfID, addrs, app.NetworkInviteOptions{
		Folders: list, AsRelay: asRelay, Admin: admin, Approve: !noApprove, TTL: ttl})
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
	if asRelay {
		role = "a temporary relay"
	}
	if admin {
		role += " and an admin"
	}
	fmt.Printf("Invitation to this network: folders %s, as %s. Valid until %s, usable once.\n",
		strings.Join(inv.Folders, ", "), role, time.Unix(inv.Exp, 0).Format("15:04:05"))
	if inv.Approve {
		fmt.Println("When the new device connects, accept it with `p2psync requests` / `p2psync accept <request>` (or on the local page).")
	}
	fmt.Println("On the new device: p2psync pair <text below> --dir <where the folders go>, or scan the QR code with the phone app.")
	fmt.Println()
	fmt.Println(code)
	fmt.Println()
	fmt.Println("Treat it like a password. It lets a device ask to join: it can only be completed with this device's agreement.")
}

// cmdPairNetwork joins a network through an admin.
func cmdPairNetwork(home string, inv pairing.Invite, dir string, yes bool) {
	_, err := app.Init(home, "", 0, "")
	check(err)
	id, err := identity.LoadOrCreate(home)
	check(err)
	cfg, err := app.LoadConfig(home)
	check(err)
	if dir == "" && cfg.NetworkDir != "" {
		dir = cfg.NetworkDir
	}
	if dir == "" {
		def, derr := os.UserHomeDir()
		if derr != nil {
			fatal(fmt.Errorf("choose where the folders go with --dir <directory>"))
		}
		dir = filepath.Join(def, "P2PSync")
	}
	dir, err = app.NormalizePath(dir)
	check(err)
	opts := app.JoinOptions{Dir: dir}

	if !yes {
		fmt.Printf("%s (%s) invites this device to its network.\n", orID(inv.Name, inv.ID), identity.Format(inv.ID))
		fmt.Printf("  folders offered : %s\n  stored in       : %s\n", strings.Join(inv.Folders, ", "), dir)
		fmt.Println("  You will trust it as a network admin: it can introduce this device to its other devices,")
		fmt.Println("  and it chooses what is shared with you. Only accept if you made this code yourself.")
		if inv.Admin {
			fmt.Println("  It also makes THIS device an admin.")
		}
		fmt.Print("Continue? [y/N] ")
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
			fatal(fmt.Errorf("cancelled"))
		}
	}
	m := app.LoadMembership(home, id, slog.New(slog.NewTextHandler(io.Discard, nil)))
	check(app.PreflightNetwork(cfg, inv))

	fmt.Printf("This device's ID is %s: the admin sees the same ID when it is asked to accept. Compare them.\n", identity.Format(id.ID))
	fmt.Printf("Connecting to %s…\n", orID(inv.Name, inv.ID))
	wait := 45 * time.Second
	if inv.Approve {
		fmt.Printf("The admin has to accept this request (up to %d minutes)…\n", int(pairing.MaxApprovalWait.Minutes()))
		wait += pairing.MaxApprovalWait
	}
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	resp, err := pairing.Dial(ctx, id.Key, inv, app.GuestRequest(cfg))
	check(err)

	// The invitation is spent now: say what to do by hand if the result cannot be recorded.
	var jerr error
	app.WithConfigLock(func() {
		if jerr = app.JoinNetwork(m, cfg, inv, resp, opts); jerr == nil {
			jerr = app.SaveConfig(home, cfg)
		}
	})
	if jerr != nil {
		fmt.Fprintf(os.Stderr, "The admin accepted this device, but the result could not be saved: %v\n", jerr)
		fmt.Fprintf(os.Stderr, "Ask for a new invitation after fixing the problem above (the previous one is spent).\n")
		os.Exit(1)
	}
	fmt.Printf("Joined the network of %s.\n", orID(resp.Name, inv.ID))
	for _, f := range cfg.Folders {
		if f.Managed {
			state := ""
			if f.AwaitingSecret {
				state = " (the folder secret arrives from a full device when it is connected)"
			}
			fmt.Printf("  folder %-16s %s%s\n", f.ID, f.Path, state)
		}
	}
	fmt.Println("If the daemon is running it reloads by itself; otherwise start it with `p2psync run`.")
}

// ---- requests ---------------------------------------------------------------------------

type request struct {
	ID       string    `json:"id"`
	DeviceID string    `json:"device_id"`
	Name     string    `json:"name"`
	IP       string    `json:"ip"`
	Folders  []string  `json:"folders"`
	AsRelay  bool      `json:"as_relay"`
	Admin    bool      `json:"admin"`
	Since    time.Time `json:"since"`
}

func listRequests(home string) []request {
	cfg := loadForEdit(home)
	if cfg.APIAddr == "" || cfg.APIAddr == "-" {
		fatal(fmt.Errorf("the status API is disabled in the configuration; it is needed to talk to the daemon"))
	}
	tok, err := app.APIToken(home)
	check(err)
	req, _ := http.NewRequest("GET", "http://"+cfg.APIAddr+"/api/requests", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := apiClient.Do(req)
	if err != nil {
		fatal(pairing.ErrNoDaemon)
	}
	defer resp.Body.Close()
	var out []request
	check(json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out))
	return out
}

func cmdRequests(home string) {
	rs := listRequests(home)
	if len(rs) == 0 {
		fmt.Println("no request is waiting")
		return
	}
	for _, r := range rs {
		extra := ""
		if r.AsRelay {
			extra += " as a relay"
		}
		if r.Admin {
			extra += " and as an admin"
		}
		fmt.Printf("%s  %q wants to join%s\n    device %s  from %s\n    folders: %s\n    accept: p2psync accept %s   reject: p2psync reject %s\n",
			r.ID, r.Name, extra, identity.Format(r.DeviceID), firstNonEmpty(r.IP, "a relayed connection"), strings.Join(r.Folders, ", "), r.ID, r.ID)
	}
}

func cmdDecide(home, id string, accept bool) {
	cfg := loadForEdit(home)
	if cfg.APIAddr == "" || cfg.APIAddr == "-" {
		fatal(fmt.Errorf("the status API is disabled in the configuration"))
	}
	tok, err := app.APIToken(home)
	check(err)
	body, _ := json.Marshal(map[string]any{"id": id, "accept": accept})
	req, _ := http.NewRequest("POST", "http://"+cfg.APIAddr+"/api/requests/decide", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := apiClient.Do(req)
	if err != nil {
		fatal(pairing.ErrNoDaemon)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNoContent:
		if accept {
			fmt.Println("accepted")
		} else {
			fmt.Println("rejected")
		}
	case http.StatusNotFound:
		fatal(fmt.Errorf("no such request (it may have timed out)"))
	default:
		fatal(fmt.Errorf("the daemon answered %s", resp.Status))
	}
}

// ---- who is who -----------------------------------------------------------------------------

func cmdNetwork(home string) {
	cfg := loadForEdit(home)
	id, err := identity.LoadOrCreate(home)
	check(err)
	role := "member"
	if cfg.Admin {
		role = "admin"
	}
	fmt.Printf("This device: %s (%s), %s\n", cfg.DeviceName, identity.Format(id.ID), role)
	if cfg.NetworkDir != "" {
		fmt.Println("Folders granted by an admin are created in:", cfg.NetworkDir)
	}
	fmt.Println("\nDevices:")
	for _, d := range cfg.Devices {
		var tags []string
		if d.Admin {
			tags = append(tags, "trusted admin")
		}
		if d.Managed {
			tags = append(tags, "introduced by an admin")
		} else {
			tags = append(tags, "paired directly")
		}
		var fs []string
		for _, f := range cfg.Folders {
			if f.SharesWith(d.ID) {
				k := f.ID
				if f.IsRelay(d.ID) {
					k += " (relay)"
				}
				fs = append(fs, k)
			}
		}
		fmt.Printf("  %-16s %s\n      %s; folders: %s\n", d.Name, identity.Format(d.ID), strings.Join(tags, ", "), firstNonEmpty(strings.Join(fs, ", "), "none"))
	}
	var waiting []string
	for _, f := range cfg.Folders {
		if f.AwaitingSecret {
			waiting = append(waiting, f.ID)
		}
	}
	if len(waiting) > 0 {
		fmt.Printf("\nWaiting for the folder secret (comes from a connected full device): %s\n", strings.Join(waiting, ", "))
	}
}
