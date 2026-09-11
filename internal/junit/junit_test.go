package junit

import (
	"strings"
	"testing"
)

const surefire = `<?xml version="1.0" encoding="UTF-8"?>
<testsuites>
  <testsuite name="org.eclipse.egit.core.test" tests="4" failures="1" errors="1" skipped="1" time="12.5">
    <testcase classname="org.eclipse.egit.core.RepositoryTest" name="clones" time="3.1"/>
    <testcase classname="org.eclipse.egit.core.RepositoryTest" name="commits" time="2.0">
      <failure message="expected:&lt;a&gt; but was:&lt;b&gt;" type="java.lang.AssertionError">stack</failure>
    </testcase>
    <testcase classname="org.eclipse.egit.core.RepositoryTest" name="fetches" time="1.0">
      <error message="SWTError: No more handles" type="org.eclipse.swt.SWTError">trace</error>
    </testcase>
    <testcase classname="org.eclipse.egit.core.RepositoryTest" name="ui" time="0">
      <skipped message="requires display"/>
    </testcase>
  </testsuite>
</testsuites>`

// A bare <testsuite> root, which the Eclipse test application also emits.
const bare = `<testsuite name="s" tests="1" failures="0" errors="0" time="1">
  <testcase classname="C" name="t" time="1"/></testsuite>`

func TestParseSurefire(t *testing.T) {
	res, err := Parse(strings.NewReader(surefire))
	if err != nil {
		t.Fatal(err)
	}
	s := res.Summary
	if s.Tests != 4 || s.Passed != 1 || s.Failed != 1 || s.Errors != 1 || s.Skipped != 1 {
		t.Fatalf("unexpected summary: %+v", s)
	}
	if got := len(res.Problems()); got != 2 {
		t.Fatalf("want 2 problems, got %d", got)
	}
	if res.Problems()[0].Message == "" {
		t.Fatal("failure message was dropped")
	}
}

func TestParseBareTestsuite(t *testing.T) {
	res, err := Parse(strings.NewReader(bare))
	if err != nil {
		t.Fatal(err)
	}
	if res.Summary.Tests != 1 || res.Summary.Passed != 1 {
		t.Fatalf("unexpected summary: %+v", res.Summary)
	}
}

func TestParseRejectsNonReport(t *testing.T) {
	if _, err := Parse(strings.NewReader(`<project><foo/></project>`)); err == nil {
		t.Fatal("expected an error for a non-report XML document")
	}
}
