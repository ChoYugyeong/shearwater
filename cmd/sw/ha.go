package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"shearwater/internal/ha"
)

const haUsage = `usage:
  sw ha status [--config-dir D] [--listen HOST:PORT] [--json]
  sw ha promote --force [--config-dir D]
  sw ha witness [--config-dir D]`

func runHA(args []string, out io.Writer) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprintln(out, haUsage)
		return nil
	}
	action := args[0]
	fs := flag.NewFlagSet("sw ha "+action, flag.ContinueOnError)
	fs.SetOutput(out)
	dir := fs.String("config-dir", defaultConfigDir, "local HA configuration directory")
	var listen string
	var jsonOut, force bool
	switch action {
	case "status":
		fs.StringVar(&listen, "listen", defaultListen, "local control plane HTTPS address")
		fs.BoolVar(&jsonOut, "json", false, "print status as JSON")
	case "promote":
		fs.BoolVar(&force, "force", false, "request manual promotion after fencing the old leader")
	case "witness":
	default:
		return opsError("INVALID", "never", "unknown HA command "+action, haUsage)
	}
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return opsError("INVALID", "never", err.Error(), haUsage)
	}
	if fs.NArg() != 0 {
		return opsError("INVALID", "never", "unexpected positional arguments", haUsage)
	}
	switch action {
	case "promote":
		cfg, err := ha.Load(*dir)
		if err != nil {
			return localConfigError(*dir, err)
		}
		if cfg.Role != ha.RoleStandby {
			return opsError("CONFLICT", "never", "only a standby can be promoted", "check sw ha status on the standby")
		}
		if !force {
			return opsError("REFUSED", "never", "manual promotion may lose writes and requires --force", "stop the old leader, then run sw ha promote --force on the standby")
		}
		fmt.Fprintln(out, "Writes made after the standby fell out of sync are lost.")
		fmt.Fprintln(out, "Never run this while the old leader may still be running.")
		if err := atomicPrivateFile(filepath.Join(*dir, ha.PromoteFile), []byte("force\n")); err != nil {
			return localConfigError(*dir, err)
		}
		fmt.Fprintln(out, "Promotion requested. The running standby checks for this request every few seconds.")
		return nil
	case "status":
		return showHAStatus(*dir, listen, jsonOut, out)
	default:
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := runWitness(ctx, *dir, out); err != nil {
			return opsError("WITNESS_ERROR", "safe", err.Error(), "check witness.json, its TLS files and the listen address")
		}
		return nil
	}
}

func readWitnessConfig(dir string) (witnessConfig, error) {
	var w witnessConfig
	err := readPrivateJSON(filepath.Join(dir, ha.WitnessFile), &w)
	if err == nil && (w.Config.Token == "" || w.Listen == "") {
		err = errors.New("witness.json needs an HA token and listen address")
	}
	return w, err
}

func runWitness(ctx context.Context, dir string, out io.Writer) error {
	lock, err := fileLock(dir, "config.lock")
	if err != nil {
		return err
	}
	defer lock.Close()
	cfg, err := readWitnessConfig(dir)
	if err != nil {
		return err
	}
	cert, err := tls.LoadX509KeyPair(filepath.Join(dir, "witness.crt"), filepath.Join(dir, "witness.key"))
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}
	defer ln.Close()
	fmt.Fprintf(out, "Witness listening on https://%s\n", ln.Addr())
	srv := &http.Server{
		Handler:           &ha.Witness{Path: filepath.Join(dir, ha.WitnessStateFile), Token: cfg.Config.Token},
		TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}},
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
	}
	done := make(chan error, 1)
	go func() { done <- srv.ServeTLS(ln, "", "") }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdown); err != nil {
			srv.Close()
			<-done
			return err
		}
		if err := <-done; !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
}

// Only public status fields belong here. Config also contains cluster secrets.
type haStatusReport struct {
	Self         string           `json:"self"`
	Role         string           `json:"role"`
	Leader       string           `json:"leader,omitempty"`
	Epoch        int64            `json:"epoch"`
	Members      []ha.Member      `json:"members,omitempty"`
	Seq          *int64           `json:"seq,omitempty"`
	InSync       *bool            `json:"inSync,omitempty"`
	Witness      string           `json:"witness,omitempty"`
	WitnessState *ha.WitnessState `json:"witnessState,omitempty"`
}

func showHAStatus(dir, listen string, jsonOut bool, out io.Writer) error {
	cfg, err := ha.Load(dir)
	var st haStatusReport
	if errors.Is(err, os.ErrNotExist) {
		w, err := readWitnessConfig(dir)
		if err != nil {
			return localConfigError(dir, err)
		}
		record := &ha.Witness{Path: filepath.Join(dir, ha.WitnessStateFile)}
		state, err := record.State()
		if err != nil {
			return localConfigError(dir, err)
		}
		st = haStatusReport{Self: w.Config.Self, Role: ha.RoleWitness, Epoch: state.Epoch,
			Members: w.Config.Members, Leader: state.Leader, Witness: "https://" + w.Listen, WitnessState: &state}
	} else if err != nil {
		return localConfigError(dir, err)
	} else {
		st = haStatusReport{Self: cfg.Self, Role: cfg.Role, Epoch: cfg.Epoch, Members: cfg.Members, Witness: cfg.Witness}
	}
	var problems []error
	if cfg.Role == ha.RoleStandby {
		var snap ha.StandbyStatus
		data, err := os.ReadFile(filepath.Join(dir, ha.StatusFile))
		if err == nil {
			err = json.Unmarshal(data, &snap)
		}
		if err == nil {
			st.Leader, st.Seq = snap.Leader, &snap.Seq
			if snap.Epoch > 0 {
				st.Epoch = snap.Epoch
			}
		} else {
			problems = append(problems, err)
		}
	}
	if cfg.Role == ha.RoleLeader || cfg.Witness != "" {
		hc, err := haHTTPClient(dir)
		if err != nil {
			return localConfigError(dir, err)
		}
		defer hc.CloseIdleConnections()
		hc.Timeout = 3 * time.Second
		hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		ctx := context.Background()
		if cfg.Role == ha.RoleLeader {
			if err := localListen(listen); err != nil {
				return err
			}
			st.Leader = cfg.Self
			c := agentClient{client: hc, url: "https://" + listen, token: cfg.Token}
			var snap ha.Status
			if err := c.get(ctx, ha.Prefix+"status", &snap); err != nil {
				problems = append(problems, err)
			} else {
				st.Leader, st.Epoch, st.Seq, st.InSync = snap.Self, snap.Epoch, &snap.Seq, &snap.InSync
			}
		}
		if cfg.Witness != "" {
			if !strings.HasPrefix(cfg.Witness, "https://") {
				return opsError("INVALID", "never", "the witness address must use HTTPS", "check ha.json")
			}
			wc := ha.WitnessClient{Client: hc, URL: cfg.Witness, Token: cfg.Token}
			reply, err := wc.Call(ctx, "state", ha.WitnessRequest{})
			switch {
			case err != nil:
				problems = append(problems, err)
			case !reply.OK:
				problems = append(problems, errors.New(reply.Reason))
			default:
				st.WitnessState = &reply.State
				if cfg.Role == ha.RoleStandby && reply.State.Epoch == st.Epoch && reply.State.Leader == st.Leader {
					sync := slices.Contains(reply.State.InSync, cfg.Self)
					st.InSync = &sync
				}
			}
		}
	}
	if err := st.print(out, jsonOut); err != nil {
		return err
	}
	if err := errors.Join(problems...); err != nil {
		return opsError("UNAVAILABLE", "safe", err.Error(), "check the local HA process and the configured witness")
	}
	return nil
}

func (st haStatusReport) print(out io.Writer, jsonOut bool) error {
	if jsonOut {
		return json.NewEncoder(out).Encode(st)
	}
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	seq, sync := "unknown", "unknown"
	if st.Seq != nil {
		seq = fmt.Sprint(*st.Seq)
	}
	if st.InSync != nil {
		sync = fmt.Sprint(*st.InSync)
	}
	fmt.Fprintln(tw, "HOST\tROLE\tLEADER\tEPOCH\tLAST BATCH\tSTANDBY IN SYNC")
	fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\n", st.Self, st.Role, st.Leader, st.Epoch, seq, sync)
	for _, m := range st.Members {
		fmt.Fprintf(tw, "Member\t%s\t%s\n", m.Name, m.URL)
	}
	if st.Witness != "" {
		fmt.Fprintf(tw, "Witness\t%s\n", st.Witness)
		if w := st.WitnessState; w != nil {
			fmt.Fprintf(tw, "Witness epoch\t%d\nWitness leader\t%s\nWitness in sync\t%s\n", w.Epoch, w.Leader, strings.Join(w.InSync, ", "))
		}
	}
	return tw.Flush()
}
