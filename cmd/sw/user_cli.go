package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
)

const userUsage = "usage: sw user add NAME [--namespace NS] [--role admin|edit|view] [--cluster-role NAME] [--out FILE] [--server URL] | list | remove NAME | token NAME [--out FILE] [--server URL]"

func runUser(args []string) error {
	if len(args) == 0 {
		return errors.New(userUsage)
	}
	action := args[0]
	if action == "-h" || action == "--help" {
		fmt.Println(userUsage)
		return nil
	}
	args = args[1:]
	fs := flag.NewFlagSet("sw user "+action, flag.ContinueOnError)
	config := fs.String("config-dir", defaultConfigDir, "administrator certificate and token directory")
	listen := fs.String("listen", defaultListen, "administrator API address")
	server := fs.String("server", "", "API address written into the kubeconfig (default: the address this control plane advertises to other machines, otherwise --listen)")
	var o userOptions
	var out string
	switch action {
	case "add":
		fs.StringVar(&o.Namespace, "namespace", "", "tenant namespace")
		fs.StringVar(&o.Role, "role", "", "namespace role (default edit)")
		fs.StringVar(&o.ClusterRole, "cluster-role", "", "additional cluster-wide role")
		fs.StringVar(&out, "out", "", "user kubeconfig path (default NAME.kubeconfig)")
	case "token":
		fs.StringVar(&out, "out", "", "write the rotated token in a user kubeconfig")
	case "remove", "list":
	default:
		return errors.New(userUsage)
	}
	if action != "list" {
		if len(args) == 0 || !validUserName(args[0]) {
			return errors.New(userUsage)
		}
		o.Name = args[0]
		args = args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New(userUsage)
	}
	if err := savedClient(fs, config, listen); err != nil {
		return err
	}
	c, err := localAPI(*config, *listen)
	if err != nil {
		return err
	}
	ctx := context.Background()
	switch action {
	case "list":
		var list struct {
			Items []userOptions `json:"items"`
		}
		if err := c.get(ctx, usersPath, &list); err != nil {
			return err
		}
		fmt.Println("NAME\tNAMESPACE\tROLE\tCLUSTER-ROLE")
		for _, item := range list.Items {
			fmt.Printf("%s\t%s\t%s\t%s\n", item.Name, item.Namespace, item.Role, item.ClusterRole)
		}
		return nil
	case "remove":
		_, _, _, err := apiRequest(ctx, c, "DELETE", usersPath+"/"+o.Name, nil)
		return err
	}
	path := usersPath
	var body []byte
	if action == "add" {
		body, err = json.Marshal(o)
		if err != nil {
			return err
		}
		if out == "" {
			out = o.Name + ".kubeconfig"
		}
	} else {
		path += "/" + o.Name + "/tokens"
	}
	var ca []byte
	if out != "" {
		ca, err = os.ReadFile(filepath.Join(*config, "ca.crt"))
		if err != nil {
			return err
		}
	}
	data, _, _, err := apiRequest(ctx, c, "POST", path, body)
	if err != nil {
		return err
	}
	var issued issuedUser
	if err := json.Unmarshal(data, &issued); err != nil {
		return err
	}
	if issued.Name != o.Name || issued.Token == "" {
		return errors.New("unexpected credential response")
	}
	if out == "" {
		fmt.Println(issued.Token)
		return nil
	}
	target := *server
	if target == "" {
		target = userServer(*config, c.url)
	}
	if err := atomicPrivateFile(out, userKubeconfig(target, ca, issued)); err != nil {
		return fmt.Errorf("kubeconfig write failed: %w; use sw user token %s --out FILE to recover", err, o.Name)
	}
	fmt.Printf("wrote kubeconfig for %s to %s\n", o.Name, out)
	return nil
}

// userServer is the API address a user's kubeconfig names. A user usually
// works from another machine, so it is the address this control plane
// advertises (sw up --advertise-address) when it has one, on the port of the
// local address the command used; otherwise that local address.
func userServer(configDir, local string) string {
	advertise := readClusterConfig(configDir).AdvertiseAddress
	if advertise == "" {
		return local
	}
	u, err := url.Parse(local)
	if err != nil {
		return local
	}
	_, port, err := net.SplitHostPort(u.Host)
	if err != nil {
		return local
	}
	return "https://" + net.JoinHostPort(advertise, port)
}

func userKubeconfig(server string, ca []byte, user issuedUser) []byte {
	namespace := user.Namespace
	if namespace == "" {
		namespace = "default"
	}
	return []byte(fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: shearwater
  cluster:
    server: %q
    certificate-authority-data: %q
users:
- name: %q
  user:
    token: %q
contexts:
- name: shearwater
  context:
    cluster: shearwater
    user: %q
    namespace: %q
current-context: shearwater
`, server, base64.StdEncoding.EncodeToString(ca), user.Name, user.Token, user.Name, namespace))
}
