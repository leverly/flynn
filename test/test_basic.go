package main

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	c "github.com/flynn/flynn/Godeps/_workspace/src/gopkg.in/check.v1"
	"github.com/flynn/flynn/cli/config"
	"github.com/flynn/flynn/controller/client"
	"github.com/flynn/flynn/pkg/attempt"
	"github.com/flynn/flynn/pkg/random"
)

type appSuite struct {
	appDir string
	ssh    *sshData
}

func (s *appSuite) initApp(t *c.C, app string) {
	s.appDir = filepath.Join(t.MkDir(), "app")
	t.Assert(run(exec.Command("cp", "-r", filepath.Join("apps", app), s.appDir)), Succeeds)
	t.Assert(s.Git("init"), Succeeds)
	t.Assert(s.Git("add", "."), Succeeds)
	t.Assert(s.Git("commit", "-am", "init"), Succeeds)
}

func (s *appSuite) Flynn(args ...string) *CmdResult {
	return flynn(s.appDir, args...)
}

func (s *appSuite) Git(args ...string) *CmdResult {
	cmd := exec.Command("git", args...)
	if s.ssh != nil {
		cmd.Env = append(os.Environ(), s.ssh.Env...)
	}
	cmd.Dir = s.appDir
	return run(cmd)
}

type BasicSuite struct {
	appSuite
	client *controller.Client
}

var _ = c.Suite(&BasicSuite{})

func (s *BasicSuite) SetUpSuite(t *c.C) {
	s.initApp(t, "basic")
	conf, err := config.ReadFile(flynnrc)
	t.Assert(err, c.IsNil)
	t.Assert(conf.Clusters, c.HasLen, 1)
	s.client = newControllerClient(t, conf.Clusters[0])
}

var Attempts = attempt.Strategy{
	Total: 20 * time.Second,
	Delay: 500 * time.Millisecond,
}

func (s *BasicSuite) TestBasic(t *c.C) {
	var err error
	s.ssh, err = genSSHKey()
	t.Assert(err, c.IsNil)
	defer s.ssh.Cleanup()

	t.Assert(s.Flynn("key", "add", s.ssh.Pub), Succeeds)

	name := random.String(30)
	t.Assert(s.Flynn("create", name), Outputs, fmt.Sprintf("Created %s\n", name))
	t.Assert(s.Flynn("apps"), OutputContains, name)
	// make sure flynn components are listed
	t.Assert(s.Flynn("apps"), OutputContains, "router")

	// keep track of job events
	stream, err := s.client.StreamJobEvents(name, 0)
	if err != nil {
		t.Error(err)
	}

	push := s.Git("push", "flynn", "master")
	t.Assert(push, Succeeds)

	t.Assert(push, OutputContains, "Node.js app detected")
	t.Assert(push, OutputContains, "Downloading and installing node")
	t.Assert(push, OutputContains, "Installing dependencies")
	t.Assert(push, OutputContains, "Procfile declares types -> web")
	t.Assert(push, OutputContains, "Creating release")
	t.Assert(push, OutputContains, "Application deployed")
	t.Assert(push, OutputContains, "* [new branch]      master -> master")

	defer s.Flynn("scale", "web=0")
	t.Assert(s.Flynn("scale", "web=3"), Succeeds)

	t.Assert(s.Flynn("env", "set", "ENV_TEST=var", "SECOND_VAL=2"), Succeeds)
	t.Assert(s.Flynn("env"), OutputContains, "ENV_TEST=var\nSECOND_VAL=2")
	t.Assert(s.Flynn("env", "get", "ENV_TEST"), Outputs, "var\n")
	// test that containers do contain the ENV var
	t.Assert(s.Flynn("run", "echo $ENV_TEST"), Outputs, "var\n")
	t.Assert(s.Flynn("env", "unset", "ENV_TEST"), Succeeds)
	t.Assert(s.Flynn("run", "echo $ENV_TEST"), Outputs, "\n")

	route := random.String(32) + ".dev"
	newRoute := s.Flynn("route", "add", "http", route)
	t.Assert(newRoute, Succeeds)
	routeID := strings.TrimSpace(newRoute.Output)
	t.Assert(s.Flynn("route"), OutputContains, routeID)

	waitForJobEventsActual(t, stream.Events, map[string]map[string]int{"web": {"up": 9, "down": 6}})

	ps := s.Flynn("ps")
	t.Assert(ps, Succeeds)
	psLines := strings.Split(strings.TrimSpace(ps.Output), "\n")
	if len(psLines) != 4 {
		t.Fatal(fmt.Errorf("Expected 4 ps lines, got %d", len(psLines)))
	}

	for _, l := range psLines[1:] {
		idType := regexp.MustCompile(`\s+`).Split(l, 2)
		if idType[1] != "web" {
			t.Fatal(fmt.Errorf("Expected web type, got %s", idType[1]))
		}
		log := s.Flynn("log", idType[0])
		if !strings.Contains(log.Output, "Listening on ") {
			t.Fatal(fmt.Errorf("Expected \"%s\" to contain \"Listening on \"", log.Output))
		}
	}

	// Make HTTP requests
	client := &http.Client{}
	req, err := http.NewRequest("GET", "http://"+routerIP, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = route
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	contents, err := ioutil.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	t.Assert(res.StatusCode, c.Equals, 200)
	t.Assert(string(contents), Matches, `Hello to Yahoo from Flynn on port \d+`)

	t.Assert(s.Flynn("route", "remove", routeID), Succeeds)
	routeOutput := s.Flynn("route").Output
	if strings.Contains(routeOutput, routeID) {
		t.Fatal(fmt.Errorf("Expected \"%s\" to not contain \"%s\"", routeOutput, routeID))
	}

	keys := s.Flynn("key")
	t.Assert(keys, Succeeds)
	k := keys.Output[:strings.Index(keys.Output, " ")]
	t.Assert(s.Flynn("key", "remove", k), Succeeds)
	keys = s.Flynn("key")
	if strings.Contains(keys.Output, k) {
		t.Fatal(fmt.Errorf("Expected \"%s\" to not contain \"%s\"", keys.Output, k))
	}
}

type BuildpackSuite struct {
	appSuite
	client *controller.Client
}

var _ = c.Suite(&BuildpackSuite{})

func (s *BuildpackSuite) SetUpSuite(t *c.C) {
	conf, err := config.ReadFile(flynnrc)
	t.Assert(err, c.IsNil)
	t.Assert(conf.Clusters, c.HasLen, 1)
	s.client = newControllerClient(t, conf.Clusters[0])
}

func (s *BuildpackSuite) TestBuildpacks(t *c.C) {
	var err error
	s.ssh, err = genSSHKey()
	t.Assert(err, c.IsNil)
	defer s.ssh.Cleanup()

	t.Assert(s.Flynn("key", "add", s.ssh.Pub), Succeeds)

	buildpacks := []struct {
		Name      string   `json:"name"`
		Resources []string `json:"resources"`
	}{
		{
			Name:      "go-flynn-example",
			Resources: []string{"postgres"},
		},
		{Name: "nodejs-flynn-example"},
		{Name: "php-flynn-example"},
		{Name: "ruby-flynn-example"},
		{Name: "java-flynn-example"},
		{Name: "clojure-flynn-example"},
		{Name: "gradle-flynn-example"},
		{Name: "grails-flynn-example"},
		{Name: "play-flynn-example"},
		{Name: "python-flynn-example"},
	}
	dir := t.MkDir()

	for _, b := range buildpacks {
		wrapErr := func(err error) error {
			return fmt.Errorf("[%q] %s", b.Name, err.Error())
		}

		s.appDir = dir
		s.Git("clone", "https://github.com/flynn-examples/"+b.Name, b.Name)
		s.appDir = filepath.Join(dir, b.Name)

		t.Assert(s.Flynn("create", b.Name), Outputs, fmt.Sprintf("Created %s\n", b.Name))

		for _, r := range b.Resources {
			t.Assert(s.Flynn("resource", "add", r), Succeeds)
		}

		push := s.Git("push", "flynn", "master")
		t.Assert(push, Succeeds)
		t.Assert(push, OutputContains, "Creating release")
		t.Assert(push, OutputContains, "Application deployed")
		t.Assert(push, OutputContains, "* [new branch]      master -> master")

		t.Assert(s.Flynn("scale", "web=1"), Succeeds)

		route := b.Name + ".dev"
		newRoute := s.Flynn("route", "add", "http", route)
		t.Assert(newRoute, Succeeds)

		if err := Attempts.Run(func() error {
			// Make HTTP requests
			client := &http.Client{}
			req, err := http.NewRequest("GET", "http://"+routerIP, nil)
			if err != nil {
				return err
			}
			req.Host = route
			res, err := client.Do(req)
			if err != nil {
				return err
			}
			defer res.Body.Close()
			contents, err := ioutil.ReadAll(res.Body)
			if err != nil {
				return err
			}
			if res.StatusCode != 200 {
				return fmt.Errorf("Expected status 200, got %v", res.StatusCode)
			}
			m, err := regexp.MatchString(`Hello from Flynn on port \d+`, string(contents))
			if err != nil {
				return err
			}
			if !m {
				return fmt.Errorf("Expected `Hello from Flynn on port \\d+`, got `%v`", string(contents))
			}
			return nil
		}); err != nil {
			t.Error(wrapErr(err))
		}
		stream, err := s.client.StreamJobEvents(b.Name, 0)
		if err != nil {
			t.Error(err)
		}
		s.Flynn("scale", "web=0")
		// wait for the jobs to stop
		waitForJobEvents(t, stream.Events, map[string]int{"web": -1})
		stream.Close()
	}
}
