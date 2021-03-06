/*
Copyright 2016 The Kubernetes Authors All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package chartutil

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	"github.com/golang/protobuf/ptypes/any"

	"k8s.io/helm/pkg/ignore"
	"k8s.io/helm/pkg/proto/hapi/chart"
)

// Load takes a string name, tries to resolve it to a file or directory, and then loads it.
//
// This is the preferred way to load a chart. It will discover the chart encoding
// and hand off to the appropriate chart reader.
//
// If a .helmignore file is present, the directory loader will skip loading any files
// matching it. But .helmignore is not evaluated when reading out of an archive.
func Load(name string) (*chart.Chart, error) {
	fi, err := os.Stat(name)
	if err != nil {
		return nil, err
	}
	if fi.IsDir() {
		return LoadDir(name)
	}
	return LoadFile(name)
}

// afile represents an archive file buffered for later processing.
type afile struct {
	name string
	data []byte
}

// LoadArchive loads from a reader containing a compressed tar archive.
func LoadArchive(in io.Reader) (*chart.Chart, error) {
	unzipped, err := gzip.NewReader(in)
	if err != nil {
		return &chart.Chart{}, err
	}
	defer unzipped.Close()

	files := []*afile{}
	tr := tar.NewReader(unzipped)
	for {
		b := bytes.NewBuffer(nil)
		hd, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return &chart.Chart{}, err
		}

		if hd.FileInfo().IsDir() {
			// Use this instead of hd.Typeflag because we don't have to do any
			// inference chasing.
			continue
		}

		parts := strings.Split(hd.Name, "/")
		n := strings.Join(parts[1:], "/")

		if parts[0] == "Chart.yaml" {
			return nil, errors.New("chart yaml not in base directory")
		}

		if _, err := io.Copy(b, tr); err != nil {
			return &chart.Chart{}, err
		}

		files = append(files, &afile{name: n, data: b.Bytes()})
		b.Reset()
	}

	if len(files) == 0 {
		return nil, errors.New("no files in chart archive")
	}

	return loadFiles(files)
}

func loadFiles(files []*afile) (*chart.Chart, error) {
	c := &chart.Chart{}
	subcharts := map[string][]*afile{}

	for _, f := range files {
		if f.name == "Chart.yaml" {
			m, err := UnmarshalChartfile(f.data)
			if err != nil {
				return c, err
			}
			c.Metadata = m
		} else if f.name == "values.toml" {
			return c, errors.New("values.toml is illegal as of 2.0.0-alpha.2")
		} else if f.name == "values.yaml" {
			c.Values = &chart.Config{Raw: string(f.data)}
		} else if strings.HasPrefix(f.name, "templates/") {
			c.Templates = append(c.Templates, &chart.Template{Name: f.name, Data: f.data})
		} else if strings.HasPrefix(f.name, "charts/") {
			if filepath.Ext(f.name) == ".prov" {
				c.Files = append(c.Files, &any.Any{TypeUrl: f.name, Value: f.data})
				continue
			}
			cname := strings.TrimPrefix(f.name, "charts/")
			if strings.IndexAny(cname, "._") == 0 {
				// Ignore charts/ that start with . or _.
				continue
			}
			parts := strings.SplitN(cname, "/", 2)
			scname := parts[0]
			subcharts[scname] = append(subcharts[scname], &afile{name: cname, data: f.data})
		} else {
			c.Files = append(c.Files, &any.Any{TypeUrl: f.name, Value: f.data})
		}
	}

	// Ensure that we got a Chart.yaml file
	if c.Metadata == nil || c.Metadata.Name == "" {
		return c, errors.New("chart metadata (Chart.yaml) missing")
	}

	for n, files := range subcharts {
		var sc *chart.Chart
		var err error
		if strings.IndexAny(n, "_.") == 0 {
			continue
		} else if filepath.Ext(n) == ".tgz" {
			file := files[0]
			if file.name != n {
				return c, fmt.Errorf("error unpacking tar in %s: expected %s, got %s", c.Metadata.Name, n, file.name)
			}
			// Untar the chart and add to c.Dependencies
			b := bytes.NewBuffer(file.data)
			sc, err = LoadArchive(b)
		} else {
			// We have to trim the prefix off of every file, and ignore any file
			// that is in charts/, but isn't actually a chart.
			buff := make([]*afile, 0, len(files))
			for _, f := range files {
				parts := strings.SplitN(f.name, "/", 2)
				if len(parts) < 2 {
					continue
				}
				f.name = parts[1]
				buff = append(buff, f)
			}
			sc, err = loadFiles(buff)
		}

		if err != nil {
			return c, fmt.Errorf("error unpacking %s in %s: %s", n, c.Metadata.Name, err)
		}

		c.Dependencies = append(c.Dependencies, sc)
	}

	return c, nil
}

// LoadFile loads from an archive file.
func LoadFile(name string) (*chart.Chart, error) {
	if fi, err := os.Stat(name); err != nil {
		return nil, err
	} else if fi.IsDir() {
		return nil, errors.New("cannot load a directory")
	}

	raw, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer raw.Close()

	return LoadArchive(raw)
}

// LoadDir loads from a directory.
//
// This loads charts only from directories.
func LoadDir(dir string) (*chart.Chart, error) {
	topdir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}

	// Just used for errors.
	c := &chart.Chart{}

	rules := ignore.Empty()
	ifile := filepath.Join(topdir, ignore.HelmIgnore)
	if _, err := os.Stat(ifile); err == nil {
		r, err := ignore.ParseFile(ifile)
		if err != nil {
			return c, err
		}
		rules = r
	}
	rules.AddDefaults()

	files := []*afile{}
	topdir += string(filepath.Separator)

	err = filepath.Walk(topdir, func(name string, fi os.FileInfo, err error) error {
		n := strings.TrimPrefix(name, topdir)

		// Normalize to / since it will also work on Windows
		n = filepath.ToSlash(n)

		if err != nil {
			return err
		}
		if fi.IsDir() {
			// Directory-based ignore rules should involve skipping the entire
			// contents of that directory.
			if rules.Ignore(n, fi) {
				return filepath.SkipDir
			}
			return nil
		}

		// If a .helmignore file matches, skip this file.
		if rules.Ignore(n, fi) {
			return nil
		}

		data, err := ioutil.ReadFile(name)
		if err != nil {
			return fmt.Errorf("error reading %s: %s", n, err)
		}

		files = append(files, &afile{name: n, data: data})
		return nil
	})
	if err != nil {
		return c, err
	}

	return loadFiles(files)
}
