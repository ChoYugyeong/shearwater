package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strings"
	"text/tabwriter"
	"time"

	"shearwater/internal/apis"
	"shearwater/internal/operation"
)

func runLease(args []string, out io.Writer) error {
	if len(args) == 0 || (args[0] != "list" && args[0] != "release") {
		return fmt.Errorf("usage: sw lease list [-n NS] | sw lease release APP [-n NS]")
	}
	action, args := args[0], args[1:]
	var app string
	if action == "release" {
		if len(args) == 0 || !apis.IsDNS1123Label(args[0]) {
			return fmt.Errorf("release requires one app name before the flags")
		}
		app, args = args[0], args[1:]
	}
	f := flag.NewFlagSet("lease "+action, flag.ContinueOnError)
	ns := f.String("n", "default", "app namespace")
	config := f.String("config-dir", defaultConfigDir, "operator credentials")
	listen := f.String("listen", defaultListen, "local HTTPS API")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 || !apis.IsDNS1123Label(*ns) {
		return fmt.Errorf("provide a valid namespace with -n")
	}
	if err := savedClient(f, config, listen); err != nil {
		return err
	}
	c, err := localAPI(*config, *listen)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	path := "/apis/coordination.k8s.io/v1/namespaces/" + *ns + "/leases"
	if action == "release" {
		path += "/" + operation.LeasePrefix + app
	}
	b, _, _, err := apiRequest(ctx, c, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	if action == "list" {
		var list struct {
			Items []apis.Lease `json:"items"`
		}
		if err = json.Unmarshal(b, &list); err != nil {
			return err
		}
		w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "APP\tHOLDER\tEXPIRES\tSTATE\tINTENT")
		now := time.Now()
		for _, l := range list.Items {
			name, ok := strings.CutPrefix(l.Metadata.Name, operation.LeasePrefix)
			if !ok || l.Spec.HolderIdentity == "" {
				continue
			}
			state := "held"
			if !now.Before(l.Expiry()) {
				state = "expired"
			}
			fmt.Fprintf(w, "%s\t%q\t%s\t%s\t%q\n", name, l.Spec.HolderIdentity, l.Expiry().UTC().Format(time.RFC3339Nano), state, l.Metadata.Annotations[operation.IntentAnnotation])
		}
		return w.Flush()
	}
	var l apis.Lease
	if err = json.Unmarshal(b, &l); err != nil {
		return err
	}
	if l.Spec.HolderIdentity == "" {
		_, err = fmt.Fprintf(out, "%s/%s is free\n", *ns, app)
		return err
	}
	holder := l.Spec.HolderIdentity
	l.Spec.HolderIdentity = ""
	delete(l.Metadata.Annotations, operation.IntentAnnotation)
	// PUT includes the observed resourceVersion. A concurrent acquisition
	// must not be released under the previously observed holder's name.
	b, err = json.Marshal(&l)
	if err != nil {
		return err
	}
	if _, _, _, err = apiRequest(ctx, c, http.MethodPut, path, b); err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "released %s/%s (held by %q)\n", *ns, app, holder)
	return err
}
