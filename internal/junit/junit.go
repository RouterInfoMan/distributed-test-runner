// Package junit parses surefire/JUnit XML as emitted by Tycho surefire and the
// Eclipse test application, and rolls it up into a model.Summary.
package junit

import (
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/andrei/distributed-test-platform/internal/model"
)

// maxDetail caps stack traces we keep in the master's store; the full text is
// always available in the uploaded artifact.
const maxDetail = 4000

type xmlSuites struct {
	XMLName xml.Name   `xml:"testsuites"`
	Suites  []xmlSuite `xml:"testsuite"`
}

type xmlSuite struct {
	XMLName  xml.Name   `xml:"testsuite"`
	Name     string     `xml:"name,attr"`
	Tests    int        `xml:"tests,attr"`
	Failures int        `xml:"failures,attr"`
	Errors   int        `xml:"errors,attr"`
	Skipped  int        `xml:"skipped,attr"`
	Time     float64    `xml:"time,attr"`
	Nested   []xmlSuite `xml:"testsuite"`
	Cases    []xmlCase  `xml:"testcase"`
}

type xmlCase struct {
	Name      string      `xml:"name,attr"`
	ClassName string      `xml:"classname,attr"`
	Time      float64     `xml:"time,attr"`
	Failure   *xmlProblem `xml:"failure"`
	Error     *xmlProblem `xml:"error"`
	Skipped   *xmlSkip    `xml:"skipped"`
}

type xmlProblem struct {
	Message string `xml:"message,attr"`
	Type    string `xml:"type,attr"`
	Text    string `xml:",chardata"`
}

type xmlSkip struct {
	Message string `xml:"message,attr"`
}

// Result is the parse of one or more report files.
type Result struct {
	Summary model.Summary
	Cases   []model.Case // every case, in file order
}

// Problems returns only the failed/errored cases, which is what the master
// stores for the dashboard.
func (r Result) Problems() []model.Case {
	var out []model.Case
	for _, c := range r.Cases {
		if c.Status == "failed" || c.Status == "error" {
			out = append(out, c)
		}
	}
	return out
}

// ParseDir walks dir and parses every file that looks like a JUnit report
// (TEST-*.xml, *junit*.xml, or any .xml whose root element matches).
func ParseDir(dir string) (Result, error) {
	var res Result
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.EqualFold(filepath.Ext(path), ".xml") {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		one, perr := Parse(f)
		if perr != nil {
			// Not every XML under the results dir is a report; skip quietly.
			return nil
		}
		res.merge(one)
		return nil
	})
	if err != nil {
		return res, err
	}
	return res, nil
}

func (r *Result) merge(o Result) {
	r.Summary.Tests += o.Summary.Tests
	r.Summary.Passed += o.Summary.Passed
	r.Summary.Failed += o.Summary.Failed
	r.Summary.Errors += o.Summary.Errors
	r.Summary.Skipped += o.Summary.Skipped
	r.Summary.Duration += o.Summary.Duration
	r.Cases = append(r.Cases, o.Cases...)
}

// Parse reads a single JUnit XML document. It accepts both a <testsuites>
// wrapper and a bare <testsuite> root, and recurses into nested suites.
func Parse(rd io.Reader) (Result, error) {
	data, err := io.ReadAll(rd)
	if err != nil {
		return Result{}, err
	}
	var res Result

	var wrapper xmlSuites
	if err := xml.Unmarshal(data, &wrapper); err == nil && wrapper.XMLName.Local == "testsuites" {
		for _, s := range wrapper.Suites {
			res.merge(fromSuite(s))
		}
		return res, nil
	}

	var single xmlSuite
	if err := xml.Unmarshal(data, &single); err == nil && single.XMLName.Local == "testsuite" {
		return fromSuite(single), nil
	}
	return Result{}, fmt.Errorf("not a junit report")
}

func fromSuite(s xmlSuite) Result {
	var res Result
	res.Summary.Duration += s.Time
	for _, c := range s.Cases {
		mc := model.Case{
			Class:    c.ClassName,
			Name:     c.Name,
			Duration: c.Time,
			Status:   "passed",
		}
		switch {
		case c.Error != nil:
			mc.Status = "error"
			mc.Message = firstNonEmpty(c.Error.Message, c.Error.Type)
			mc.Details = trunc(c.Error.Text)
			res.Summary.Errors++
		case c.Failure != nil:
			mc.Status = "failed"
			mc.Message = firstNonEmpty(c.Failure.Message, c.Failure.Type)
			mc.Details = trunc(c.Failure.Text)
			res.Summary.Failed++
		case c.Skipped != nil:
			mc.Status = "skipped"
			mc.Message = c.Skipped.Message
			res.Summary.Skipped++
		default:
			res.Summary.Passed++
		}
		res.Summary.Tests++
		res.Cases = append(res.Cases, mc)
	}
	for _, n := range s.Nested {
		res.merge(fromSuite(n))
	}
	return res
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func trunc(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > maxDetail {
		return s[:maxDetail] + "\n... (truncated, see artifact)"
	}
	return s
}
