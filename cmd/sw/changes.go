package main

// sw changes: what changed since a revision, and who changed it
// (design-agent-first-ha 2.3.2). The server summarizes its change log; the
// command only asks and prints, so an agent can keep the `next` revision and
// ask again later for just the difference.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"text/tabwriter"

	"shearwater/internal/api"
	"shearwater/internal/apis"
)

type changesQuery struct {
	Since     int64 // negative: the newest entries
	Namespace string
	Actor     string
	Limit     int
	Status    bool
}

func (q changesQuery) path() string {
	v := url.Values{}
	if q.Since >= 0 {
		v.Set("since", strconv.FormatInt(q.Since, 10))
	}
	if q.Namespace != "" {
		v.Set("namespace", q.Namespace)
	}
	if q.Actor != "" {
		v.Set("actor", q.Actor)
	}
	if q.Limit > 0 {
		v.Set("limit", strconv.Itoa(q.Limit))
	}
	if q.Status {
		v.Set("status", "true")
	}
	if len(v) == 0 {
		return api.ChangesPath
	}
	return api.ChangesPath + "?" + v.Encode()
}

func runChanges(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("sw changes", flag.ContinueOnError)
	fs.SetOutput(out)
	config := fs.String("config-dir", defaultConfigDir, "directory with the server CA and token")
	listen := fs.String("listen", defaultListen, "local HTTPS API")
	q := changesQuery{}
	fs.Int64Var(&q.Since, "since", -1, "show the changes after this revision (default: the newest ones)")
	fs.StringVar(&q.Namespace, "n", "", "only this namespace")
	fs.StringVar(&q.Namespace, "namespace", "", "only this namespace")
	fs.StringVar(&q.Actor, "actor", "", "only changes made by this user, agent, node (system:node:NAME) or \"system\"")
	fs.IntVar(&q.Limit, "limit", 50, "at most this many changes (1-200)")
	fs.BoolVar(&q.Status, "status", false, "also list status-only updates and heartbeats")
	asJSON := fs.Bool("json", false, "print the server's JSON answer")
	fs.Usage = func() {
		fmt.Fprintln(out, "usage: sw changes [--since REV] [-n NS] [--actor NAME] [--limit N] [--status] [--json]")
		fmt.Fprintln(out, "Lists what changed after a revision and who changed it; repeat with the printed --since to see only newer changes.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 {
		return opsError("INVALID", "never", "unexpected argument "+strconv.Quote(fs.Arg(0)), "use flags only, for example sw changes --since 120")
	}
	if q.Limit < 1 || q.Limit > 200 {
		return opsError("INVALID", "never", fmt.Sprintf("--limit must be 1 to 200, got %d", q.Limit), "")
	}
	c, err := localAPI(*config, *listen)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), inspectTimeout)
	defer cancel()
	list, raw, err := fetchChanges(ctx, c, q)
	if err != nil {
		return err
	}
	if *asJSON {
		_, err := fmt.Fprintln(out, strings.TrimSpace(string(raw)))
		return err
	}
	return printChanges(out, list)
}

// fetchChanges asks the server and keeps its error text, which carries the
// error code and hint (for example COMPACTED: read the state again).
func fetchChanges(ctx context.Context, c *agentClient, q changesQuery) (api.ChangeList, []byte, error) {
	var list api.ChangeList
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url+q.path(), nil)
	if err != nil {
		return list, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	res, err := c.client.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return list, nil, opsError("TIMEOUT", "safe", "no reply from "+c.url, "check sw doctor and the server log")
		}
		return list, nil, opsError("NOT_RUNNING", "never", fmt.Sprintf("cannot reach %s: %v", c.url, err), "run sw up or check --listen and sw doctor")
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		return list, nil, err
	}
	if res.StatusCode != http.StatusOK {
		var st apis.Status
		if json.Unmarshal(body, &st) == nil && st.Message != "" {
			return list, nil, errors.New(st.Message)
		}
		return list, nil, opsError("RUNTIME_ERROR", "safe", fmt.Sprintf("%s replied HTTP %d", c.url, res.StatusCode), "check sw doctor")
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return list, nil, err
	}
	return list, body, nil
}

func printChanges(out io.Writer, list api.ChangeList) error {
	if len(list.Changes) == 0 {
		fmt.Fprintln(out, "No changes to show.")
	} else {
		w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "REV\tTIME\tACTOR\tVIA\tCHANGE\tOBJECT\tFIELDS")
		for _, c := range list.Changes {
			obj := c.Kind + " " + c.Name
			if c.Namespace != "" {
				obj = c.Kind + " " + c.Namespace + "/" + c.Name
			}
			via := c.Via
			if via == "" {
				via = "-"
			}
			fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\t%s\n", c.Rev, c.At, c.Actor, via, c.Type, obj, strings.Join(c.Fields, ", "))
		}
		if err := w.Flush(); err != nil {
			return err
		}
	}
	if len(list.Folded) > 0 {
		var parts []string
		for _, k := range []string{"status", "heartbeat"} {
			if n := list.Folded[k]; n > 0 {
				parts = append(parts, fmt.Sprintf("%d %s", n, k))
			}
		}
		fmt.Fprintf(out, "Folded: %s (--status lists them).\n", strings.Join(parts, ", "))
	}
	more := ""
	if list.More {
		more = " (more may follow)"
	}
	_, err := fmt.Fprintf(out, "Next: sw changes --since %d%s\n", list.Next, more)
	return err
}

// mcpChanges answers the sw_changes tool with the server's bounded JSON.
func mcpChanges(o mcpOptions, args map[string]json.RawMessage) (string, error) {
	q := changesQuery{Since: -1, Limit: 50}
	if b, ok := args["since"]; ok {
		if err := json.Unmarshal(b, &q.Since); err != nil || q.Since < 0 {
			return "", opsError("INVALID", "never", "since must be a non-negative revision", "pass the next value of the previous answer")
		}
	}
	if b, ok := args["namespace"]; ok {
		json.Unmarshal(b, &q.Namespace)
	}
	if b, ok := args["actor"]; ok {
		json.Unmarshal(b, &q.Actor)
	}
	if b, ok := args["status"]; ok {
		json.Unmarshal(b, &q.Status)
	}
	c, err := localAPI(o.Config, o.Listen)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), inspectTimeout)
	defer cancel()
	_, raw, err := fetchChanges(ctx, c, q)
	return strings.TrimSpace(string(raw)), err
}
