// Package nomad is a minimal HTTP client for the Nomad API: enough to read the
// node inventory, register and stop batch jobs, and follow allocation state.
// Jobs are submitted as JSON (map-shaped) so the platform does not have to
// model Nomad's full job struct.
package nomad

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Client struct {
	Address   string
	Token     string
	Namespace string
	Region    string
	HTTP      *http.Client
}

func New(address, token, namespace, region string) *Client {
	return &Client{
		Address:   strings.TrimRight(address, "/"),
		Token:     token,
		Namespace: namespace,
		Region:    region,
		HTTP:      &http.Client{Timeout: 30 * time.Second},
	}
}

// ---------------------------------------------------------------------------
// read types (only the fields the platform uses)
// ---------------------------------------------------------------------------

type NodeStub struct {
	ID                    string `json:"ID"`
	Name                  string `json:"Name"`
	Datacenter            string `json:"Datacenter"`
	NodeClass             string `json:"NodeClass"`
	NodePool              string `json:"NodePool"`
	Status                string `json:"Status"`
	SchedulingEligibility string `json:"SchedulingEligibility"`
	Drain                 bool   `json:"Drain"`
}

type Node struct {
	NodeStub
	Meta          map[string]string `json:"Meta"`
	Attributes    map[string]string `json:"Attributes"`
	NodeResources *NodeResources    `json:"NodeResources"`
}

// NodeResources is the fingerprinted capacity the scheduler places against.
type NodeResources struct {
	Cpu    CPUResources `json:"Cpu"`
	Memory struct {
		MemoryMB int64 `json:"MemoryMB"`
	} `json:"Memory"`
}

type CPUResources struct {
	CpuShares          int64 `json:"CpuShares"`          // MHz of compute
	ReservableCpuCores []int `json:"ReservableCpuCores"` // cores a task may pin
}

// Ready reports whether the node can accept work right now.
func (n Node) Ready() bool {
	return n.Status == "ready" && !n.Drain && n.SchedulingEligibility != "ineligible"
}

type Alloc struct {
	ID                 string            `json:"ID"`
	JobID              string            `json:"JobID"`
	Name               string            `json:"Name"`
	NodeID             string            `json:"NodeID"`
	NodeName           string            `json:"NodeName"`
	ClientStatus       string            `json:"ClientStatus"`
	ClientDescription  string            `json:"ClientDescription"`
	DesiredStatus      string            `json:"DesiredStatus"`
	DesiredDescription string            `json:"DesiredDescription"`
	TaskStates         map[string]*State `json:"TaskStates"`
	CreateIndex        uint64            `json:"CreateIndex"`
}

type State struct {
	State     string  `json:"State"`
	Failed    bool    `json:"Failed"`
	StartedAt string  `json:"StartedAt"`
	Events    []Event `json:"Events"`
}

type Event struct {
	Type           string            `json:"Type"`
	Time           int64             `json:"Time"`
	DisplayMessage string            `json:"DisplayMessage"`
	ExitCode       int               `json:"ExitCode"`
	Message        string            `json:"Message"`
	Details        map[string]string `json:"Details"` // drivers add exit_code, signal, oom_killed
}

// ExitCode returns the task's exit code when it has finished.
func (a *Alloc) ExitCode() (int, bool) {
	for _, ts := range a.TaskStates {
		for i := len(ts.Events) - 1; i >= 0; i-- {
			if ts.Events[i].Type == "Terminated" {
				return ts.Events[i].ExitCode, true
			}
		}
	}
	return 0, false
}

// FailureMessage surfaces the most useful description of why an alloc failed.
func (a *Alloc) FailureMessage() string {
	// The task's own end is the most specific thing Nomad knows; an OOM kill
	// in particular looks like a plain non-zero exit everywhere else.
	for _, ts := range a.TaskStates {
		for i := len(ts.Events) - 1; i >= 0; i-- {
			e := ts.Events[i]
			switch e.Type {
			case "Terminated":
				if e.Details["oom_killed"] == "true" {
					return fmt.Sprintf("OOM killed: the task exceeded its memory limit (exit %d)", e.ExitCode)
				}
				if e.ExitCode != 0 {
					return fmt.Sprintf("task exited %d without reporting results", e.ExitCode)
				}
			case "Driver Failure", "Setup Failure", "Killing":
				if e.DisplayMessage != "" {
					return e.DisplayMessage
				}
				return e.Message
			}
		}
	}
	if a.ClientDescription != "" {
		return a.ClientDescription
	}
	return a.DesiredDescription
}

// ---------------------------------------------------------------------------
// operations
// ---------------------------------------------------------------------------

func (c *Client) Nodes(ctx context.Context) ([]NodeStub, error) {
	var out []NodeStub
	err := c.do(ctx, http.MethodGet, "/v1/nodes", nil, nil, &out)
	return out, err
}

func (c *Client) Node(ctx context.Context, id string) (*Node, error) {
	var out Node
	if err := c.do(ctx, http.MethodGet, "/v1/node/"+id, nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RegisterJob submits a job definition and returns the evaluation ID.
func (c *Client) RegisterJob(ctx context.Context, job map[string]any) (string, error) {
	body := map[string]any{"Job": job}
	var resp struct {
		EvalID   string `json:"EvalID"`
		Warnings string `json:"Warnings"`
	}
	if err := c.do(ctx, http.MethodPost, "/v1/jobs", nil, body, &resp); err != nil {
		return "", err
	}
	return resp.EvalID, nil
}

// StopJob deregisters a job; purge removes it from state entirely.
func (c *Client) StopJob(ctx context.Context, jobID string, purge bool) error {
	q := url.Values{}
	if purge {
		q.Set("purge", "true")
	}
	return c.do(ctx, http.MethodDelete, "/v1/job/"+url.PathEscape(jobID), q, nil, nil)
}

func (c *Client) JobAllocations(ctx context.Context, jobID string) ([]Alloc, error) {
	var out []Alloc
	err := c.do(ctx, http.MethodGet, "/v1/job/"+url.PathEscape(jobID)+"/allocations", nil, nil, &out)
	return out, err
}

// Allocations lists every allocation in the namespace, which the scheduler uses
// to reconcile its slot ledger against reality.
func (c *Client) Allocations(ctx context.Context, prefix string) ([]Alloc, error) {
	q := url.Values{}
	if prefix != "" {
		q.Set("prefix", prefix)
	}
	var out []Alloc
	err := c.do(ctx, http.MethodGet, "/v1/allocations", q, nil, &out)
	return out, err
}

// AllocLogs fetches stdout/stderr for a task in an allocation.
func (c *Client) AllocLogs(ctx context.Context, allocID, task, logType string, offset int64) ([]byte, error) {
	q := url.Values{}
	q.Set("task", task)
	q.Set("type", logType)
	q.Set("plain", "true")
	q.Set("origin", "start")
	q.Set("offset", fmt.Sprintf("%d", offset))
	req, err := c.newReq(ctx, http.MethodGet, "/v1/client/fs/logs/"+allocID, q, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, errFrom("logs", resp)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

// Ping reports whether the Nomad API is reachable.
func (c *Client) Ping(ctx context.Context) error {
	var out map[string]any
	return c.do(ctx, http.MethodGet, "/v1/agent/self", nil, nil, &out)
}

func (c *Client) newReq(ctx context.Context, method, path string, q url.Values, body any) (*http.Request, error) {
	if q == nil {
		q = url.Values{}
	}
	if c.Namespace != "" {
		q.Set("namespace", c.Namespace)
	}
	if c.Region != "" {
		q.Set("region", c.Region)
	}
	u := c.Address + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		req.Header.Set("X-Nomad-Token", c.Token)
	}
	return req, nil
}

func (c *Client) do(ctx context.Context, method, path string, q url.Values, body, out any) error {
	req, err := c.newReq(ctx, method, path, q, body)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return errFrom(method+" "+path, resp)
	}
	if out == nil {
		io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func errFrom(op string, resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return fmt.Errorf("nomad %s: %s: %s", op, resp.Status, strings.TrimSpace(string(b)))
}
