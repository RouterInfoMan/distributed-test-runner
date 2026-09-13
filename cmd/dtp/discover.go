package main

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/andrei/distributed-test-platform/internal/model"
)

// discover: a Tycho reactor turned into a submission.

// doDiscover is the splitter: every eclipse-test-plugin module in a Tycho
// reactor becomes one suite of the printed submission.
func doDiscover(dir, pool, build string) error {
	type pom struct {
		ArtifactID string `xml:"artifactId"`
		Packaging  string `xml:"packaging"`
	}
	var suites []model.SuiteSpec
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == "target" || strings.HasPrefix(d.Name(), ".")) && p != dir {
			return filepath.SkipDir
		}
		if d.IsDir() || d.Name() != "pom.xml" {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		var pm pom
		if xml.Unmarshal(b, &pm) != nil || pm.Packaging != "eclipse-test-plugin" {
			return nil
		}
		name := pm.ArtifactID
		if name == "" {
			name = filepath.Base(filepath.Dir(p))
		}
		suites = append(suites, model.SuiteSpec{Name: name})
		return nil
	})
	if err != nil {
		return err
	}
	if len(suites) == 0 {
		return fmt.Errorf("no eclipse-test-plugin modules under %s", dir)
	}
	sort.Slice(suites, func(i, j int) bool { return suites[i].Name < suites[j].Name })
	abs, _ := filepath.Abs(dir)
	sub := model.Submission{
		Name: filepath.Base(abs),
		Defaults: model.SuiteDefaults{
			Pool:    pool,
			Timeout: model.Duration(45 * time.Minute),
			Build: []model.BuildArtifact{{
				Name: build, URL: "${BUILD_URL}", SHA256: "${BUILD_SHA256}", Unpack: "tar.gz",
			}},
			Env: map[string]string{"DTP_HARNESS": "tycho", "DTP_TYCHO_BUILD": build},
		},
		Suites: suites,
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(sub)
}
