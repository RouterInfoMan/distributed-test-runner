package yamlite

import (
	"reflect"
	"testing"
)

type nodeCfg struct {
	Master  string            `json:"master"`
	Pool    string            `json:"pool"`
	Slots   any               `json:"slots"`
	Labels  map[string]string `json:"labels"`
	Spans   []string          `json:"spans"`
	Reserve struct {
		Cores    int `json:"cores"`
		MemoryMB int `json:"memory_mb"`
	} `json:"reserve"`
	Tags   []string `json:"tags"`
	Debug  bool     `json:"debug"`
	Groups []struct {
		Name  string   `json:"name"`
		Users []string `json:"users"`
		Max   int      `json:"max"`
	} `json:"groups"`
}

const doc = `
# a node
master: http://master:8080   # trailing comment
pool: "high-perf-pool"
slots: auto
labels:
  rcp_version: "4.36"     # quoted: stays a string
  perf: high
  note: 'it''s fine: yes'
reserve:
  cores: 1
  memory_mb: 1024
spans: [hp, "mid pool"]
tags:
  - a
  - "b # not a comment"
debug: yes
groups:
  - name: core
    users: [alice, bob]
    max: 6
  - name: release
    max: 8
`

func TestUnmarshal(t *testing.T) {
	var c nodeCfg
	if err := Unmarshal([]byte(doc), &c); err != nil {
		t.Fatal(err)
	}
	if c.Master != "http://master:8080" || c.Pool != "high-perf-pool" || c.Slots != "auto" || !c.Debug {
		t.Fatalf("scalars: %+v", c)
	}
	if c.Labels["rcp_version"] != "4.36" || c.Labels["perf"] != "high" || c.Labels["note"] != "it's fine: yes" {
		t.Fatalf("labels: %+v", c.Labels)
	}
	if c.Reserve.Cores != 1 || c.Reserve.MemoryMB != 1024 {
		t.Fatalf("nested ints: %+v", c.Reserve)
	}
	if !reflect.DeepEqual(c.Spans, []string{"hp", "mid pool"}) || !reflect.DeepEqual(c.Tags, []string{"a", "b # not a comment"}) {
		t.Fatalf("sequences: %v %v", c.Spans, c.Tags)
	}
	if len(c.Groups) != 2 || c.Groups[0].Name != "core" || c.Groups[0].Max != 6 ||
		!reflect.DeepEqual(c.Groups[0].Users, []string{"alice", "bob"}) || c.Groups[1].Name != "release" {
		t.Fatalf("sequence of mappings: %+v", c.Groups)
	}
}

func TestNumbersStayNumbers(t *testing.T) {
	var out struct {
		Slots int `json:"slots"`
	}
	if err := Unmarshal([]byte("slots: 6\n"), &out); err != nil || out.Slots != 6 {
		t.Fatalf("plain 6 must decode into an int: %v %+v", err, out)
	}
	var str struct {
		Version string `json:"version"`
	}
	if err := Unmarshal([]byte("version: \"4.36\"\n"), &str); err != nil || str.Version != "4.36" {
		t.Fatalf("quoted number stays a string: %v %+v", err, str)
	}
}

func TestJSONPassthrough(t *testing.T) {
	var c nodeCfg
	if err := Unmarshal([]byte(`{"master":"http://m","labels":{"a":"b"}}`), &c); err != nil || c.Labels["a"] != "b" {
		t.Fatalf("json: %v %+v", err, c)
	}
}

func TestErrors(t *testing.T) {
	for name, bad := range map[string]string{
		"tab indent":   "labels:\n\tperf: high\n",
		"no key":       "just text\n",
		"flow mapping": "labels: {a: b}\n",
		"bad indent":   "labels:\n    a: b\n  c: d\n",
	} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}
