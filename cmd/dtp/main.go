// Command dtp is the operator CLI: submit a regression, watch it, inspect
// pools, pull artifacts.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const usage = `dtp - distributed test platform CLI

usage:
  dtp submit <submission.json> [-w] [-id ID] [-priority N] [-user NAME]
  dtp status <regression-id> [-w]
  dtp list
  dtp pools
  dtp quotas                                   # the rule table with live usage
  dtp quotas set <rule> <max-slots> [note]     # add or change a rule; 0 forbids
  dtp quotas rm <rule>
  dtp builds                                   # the build repository: payloads, versions, suites
  dtp nodes                                    # every node, its pool, slots and slot size
  dtp nodes assign <node> [<pool>]             # put a node in a pool (no pool: unassign)
  dtp cancel <regression-id> [-suite NAME]
  dtp artifacts <run-id> [-get PATH]
  dtp logs <run-id>
  dtp discover <reactor-dir> [-pool NAME] [-build NAME]
  dtp config                                   # the catalog as stored: pools, nodes, groups, users, quota rules
  dtp config apply <catalog.json>              # replace it with a {"pools", "nodes", "groups", "users", "quotas"} file
  dtp config reload                            # re-read the store after editing it with SQL

global flags:
  -master URL   master address (default $DTP_MASTER or http://127.0.0.1:8080)

'submit -user' names who is charged for the slots (default $DTP_USER, then
$USER); the master charges the slots to that user and applies their rules.

'discover' scans a Tycho reactor for eclipse-test-plugin modules and prints a
submission that runs each one as a suite; pipe it to a file and submit it.

'config' shows the catalog - pools, node assignments, groups, users and quota
rules - which lives in the master's store (PostgreSQL tables or catalog.json),
not in the config file; 'config apply' replaces it, validated, with immediate
effect; 'nodes assign' moves one node; 'quotas set' writes one rule. Every
write is idempotent: applying what is already there changes nothing.

A quota rule is (user | group | everyone) x (node | pool | everywhere) -> max
slots, written as subject[:name][/together]@scope[:target]:
  global@global                     each user, anywhere
  user:carol@global                 carol, anywhere (beats her groups' rules)
  group:release@pool:high-perf-pool each member of release, in that pool
  group:core-devs/together@global   all of core-devs added up
  global/together@node:rcp-hp-1     everyone added up, on that node

'submit -w' and 'status -w' follow the run and exit non-zero if it did not
pass, which is what a CI job wants.
`

var (
	master  string
	posArgs []string
)

func main() {
	// -master is accepted both before and after the subcommand, since it reads
	// naturally either way in a CI script.
	argv, preMaster := extractGlobalMaster(os.Args[1:])
	if len(argv) < 1 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd := argv[0]
	args := argv[1:]

	def := envOr("DTP_MASTER", "http://127.0.0.1:8080")
	if preMaster != "" {
		def = preMaster
	}
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	fs.StringVar(&master, "master", def, "master address")

	var err error
	switch cmd {
	case "submit":
		watch := fs.Bool("w", false, "wait for completion")
		id := fs.String("id", "", "override regression id")
		prio := fs.Int("priority", 0, "override priority")
		user := fs.String("user", envOr("DTP_USER", os.Getenv("USER")), "user charged for the slots")
		parse(fs, args, 1)
		err = doSubmit(arg(0), *id, *prio, *user, *watch)
	case "status":
		watch := fs.Bool("w", false, "follow until complete")
		parse(fs, args, 1)
		err = doStatus(arg(0), *watch)
	case "list":
		parse(fs, args, 0)
		err = doList()
	case "pools":
		parse(fs, args, 0)
		err = doPools()
	case "quotas":
		parse(fs, args, 0)
		err = doQuotas(posArgs)
	case "builds":
		parse(fs, args, 0)
		err = doBuilds()
	case "nodes":
		parse(fs, args, 0)
		err = doNodes(posArgs)
	case "cancel":
		suite := fs.String("suite", "", "cancel only this suite of the regression")
		parse(fs, args, 1)
		err = doCancel(arg(0), *suite)
	case "config":
		parse(fs, args, 0)
		switch arg(0) {
		case "apply":
			if arg(1) == "" {
				fmt.Fprint(os.Stderr, usage)
				os.Exit(2)
			}
			err = doConfigApply(arg(1))
		case "reload":
			var out map[string]any
			if err = call(http.MethodPost, "/api/v1/config/reload", nil, &out); err == nil {
				fmt.Println("catalog reloaded from the store")
			}
		default:
			err = doConfig()
		}
	case "discover":
		pool := fs.String("pool", "global", "pool for every suite")
		build := fs.String("build", "reactor", "name of the build payload carrying the reactor")
		parse(fs, args, 1)
		err = doDiscover(arg(0), *pool, *build)
	case "artifacts":
		get := fs.String("get", "", "download this artifact path to stdout")
		parse(fs, args, 1)
		err = doArtifacts(arg(0), *get)
	case "logs":
		parse(fs, args, 1)
		err = doArtifacts(arg(0), "dtp-runner.log")
	case "-h", "--help", "help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "dtp:", err)
		os.Exit(1)
	}
}

// extractGlobalMaster pulls a leading "-master URL" / "-master=URL" out of the
// argument list so it can precede the subcommand.
func extractGlobalMaster(args []string) ([]string, string) {
	val := ""
	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		a := strings.TrimLeft(args[0], "-")
		switch {
		case a == "master" && len(args) > 1:
			val, args = args[1], args[2:]
		case strings.HasPrefix(a, "master="):
			val, args = strings.TrimPrefix(a, "master="), args[1:]
		default:
			return args, val // -h and friends fall through to the dispatcher
		}
	}
	return args, val
}

// parse permutes flags and positional arguments so "dtp artifacts RUN -get X"
// works as naturally as "dtp artifacts -get X RUN"; Go's flag package stops at
// the first non-flag argument on its own.
func parse(fs *flag.FlagSet, args []string, want int) {
	posArgs = nil
	for {
		if err := fs.Parse(args); err != nil {
			os.Exit(2)
		}
		if fs.NArg() == 0 {
			break
		}
		posArgs = append(posArgs, fs.Arg(0))
		args = fs.Args()[1:]
	}
	if len(posArgs) < want {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
}

// arg returns the nth positional argument, or "".
func arg(n int) string {
	if n < len(posArgs) {
		return posArgs[n]
	}
	return ""
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func call(method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, strings.TrimRight(master, "/")+path, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("contacting master at %s: %w", master, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			return fmt.Errorf("%s", e.Error)
		}
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
