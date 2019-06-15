package main

import (
	"crypto/sha256"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path"
	"regexp"
	"strings"
	"sync"
	"text/template"

	"gopkg.in/yaml.v2"
)

const (
	pythonVersion  = "3.6"
	distroListURL  = "https://raw.githubusercontent.com/ros/rosdistro/master/melodic/distribution.yaml"
	githubRawURL   = "https://raw.githubusercontent.com"
	outputPath     = "out"
	goTemplateName = "default.tmpl"
)

type SubPackage struct {
	Name              string   `xml:"name"`
	Description       string   `xml:"description"`
	BuildDependencies []string `xml:"buildtool_depend"`
	RunDependencies   []string `xml:"run_depend"`
}

type RepoData struct {
	// From distribution.yaml
	Name string
	Doc  struct {
		Type    string
		URL     string
		Version string
	}
	Release struct {
		Packages []string
		Tags     map[string]string
		URL      string
		Version  string
	}
	Source struct {
		Type    string
		URL     string
		Version string
	}
	Status string

	// From package.xml
	SubPackages []*SubPackage

	// Custom
	TarballURL string
	CheckSum   string
}

type DistroData struct {
	ReleasePlatforms struct {
		Debian []string
		Fedora []string
		Ubuntu []string
	} `yaml:"release_platforms"`

	Repositories map[string]RepoData
	Version      string
	Type         string
}

type Settings struct {
	Maintainer string
}

func Error(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

func formatPackageName(s string) string {
	s = strings.ReplaceAll(s, "_", "-")
	return s
}

func formatVersionString(s string) string {
	s = strings.ReplaceAll(s, "-", "_")
	s = strings.ReplaceAll(s, ":", "_")
	return s
}

func formatDescription(s string) string {
	s = strings.Trim(s, " .\n")
	if len(s)+6 >= 72 {
		s = s[0:62] + "..."
	}
	return s
}

func formatDependencyList(ss []string, offset, indent int, first bool) string {
	var sb strings.Builder
	// col starts out at 9 because we assume it's used in `depends=`
	col := offset

	ignoreList := map[string]bool{
		"cmake":   true,
		"python3": true,
		"python":  true,
		"catkin":  true,
	}

	for _, s := range ss {
		if _, ok := ignoreList[s]; ok {
			continue
		}

		s := "ros-melodic-" + formatPackageName(s)
		if col+len(s)+1 > 100 {
			sb.WriteString("\n")
			col = 1
			if indent == 1 {
				sb.WriteString("\t")
				col += 2
			}
		}
		if !first {
			sb.WriteString(" ")
			col++
		}
		first = false
		sb.WriteString(s)
		col += len(s)
	}
	return sb.String()
}

func getHTTPResponseBody(url string) ([]byte, error) {
	resp, err := http.Get(url)
	defer resp.Body.Close()
	if err != nil {
		return nil, err
	}
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	return body, nil
}

func getPackageList() DistroData {
	d := DistroData{}

	body, err := getHTTPResponseBody(distroListURL)
	Error(err)

	err = yaml.Unmarshal(body, &d)
	Error(err)

	return d
}

func parseGoTemplate() *template.Template {
	t, err := template.New(goTemplateName).Funcs(
		template.FuncMap{
			"fmt":        formatPackageName,
			"fmtDesc":    formatDescription,
			"fmtVersion": formatVersionString,
			"fmtList":    formatDependencyList,
		},
	).ParseFiles(goTemplateName)
	Error(err)
	return t
}

func openVoidTemplateFile(name string) *os.File {
	p := path.Join(outputPath, name)
	if _, err := os.Stat(p); os.IsNotExist(err) {
		os.Mkdir(p, os.ModePerm)
	}

	f, err := os.Create(path.Join(p, "template"))
	Error(err)

	return f
}

func getGithubRepoFromURL(url string) (string, error) {
	re := regexp.MustCompile(`([^/]+/[^/^.]+)\.git?`)
	if re.MatchString(url) {
		return string(re.FindStringSubmatch(url)[1]), nil
	} else {
		return "", errors.New("Not a correct github URL.")
	}
}

func getPackageXML(name, version, url string) (*SubPackage, error) {
	githubRepo, err := getGithubRepoFromURL(url)
	if err != nil {
		return nil, err
	}

	sp := &SubPackage{}
	rawurl := fmt.Sprintf("%s/%s/%s/%s/package.xml", githubRawURL, githubRepo, version, name)
	body, err := getHTTPResponseBody(rawurl)
	xml.Unmarshal(body, sp)

	return sp, err
}

func getTarballURL(name, version, url string) string {
	return fmt.Sprintf(
		"%s/archive/release/%s/%s/%s.tar.gz",
		strings.ReplaceAll(url, ".git", ""),
		"melodic",
		name,
		version,
	)
}

func getTarballChecksum(url string) (string, error) {
	body, err := getHTTPResponseBody(url)
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("%x", sha256.Sum256(body)), nil
}

func prepareAdditionalPackageData(pkgname string, repodata *RepoData) error {
	var pkgxml *SubPackage
	var err error

	if len(repodata.Release.Packages) == 0 {
		pkgxml, err = getPackageXML(pkgname, repodata.Source.Version, repodata.Source.URL)
		repodata.SubPackages = append(repodata.SubPackages, pkgxml)
	} else {
		for _, subpkgname := range repodata.Release.Packages {
			pkgxml, err = getPackageXML(subpkgname, repodata.Source.Version, repodata.Source.URL)
			repodata.SubPackages = append(repodata.SubPackages, pkgxml)
		}
	}

	return err
}

func generateTemplate(pkgname string, repodata *RepoData, tmpl *template.Template) {
	var err error
	repodata.Name = pkgname
	repodata.TarballURL = getTarballURL(pkgname, repodata.Release.Version, repodata.Release.URL)
	println(repodata.TarballURL)
	repodata.CheckSum, err = getTarballChecksum(repodata.TarballURL)
	Error(err)

	if len(repodata.Release.URL) > 0 {
		err := prepareAdditionalPackageData(pkgname, repodata)
		if err == nil {
			f := openVoidTemplateFile("ros-melodic-" + formatPackageName(pkgname))
			err = tmpl.ExecuteTemplate(f, goTemplateName, repodata)
			if err != nil {
				println("ERROR AT " + pkgname)
				Error(err)
			}
		}
	}
}

func main() {
	d := getPackageList()
	t := parseGoTemplate()

	name := flag.String("p", "", "package name")
	flag.Parse()

	if len(*name) == 0 {
		var wg sync.WaitGroup
		wg.Add(len(d.Repositories))
		for pkgname, repodata := range d.Repositories {
			go func(pkgname string, repodata RepoData) {
				generateTemplate(pkgname, &repodata, t)
				wg.Done()
			}(pkgname, repodata)
		}
		wg.Wait()
	} else {
		println("Single Mode: generating " + *name)
		if repodata, ok := d.Repositories[*name]; ok {
			generateTemplate(*name, &repodata, t)
		}
	}
}
