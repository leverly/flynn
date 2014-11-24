package main

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io/ioutil"
	"strings"
	"time"

	c "github.com/flynn/flynn/Godeps/_workspace/src/github.com/flynn/go-check"
	"github.com/flynn/flynn/Godeps/_workspace/src/golang.org/x/crypto/ssh"
	ct "github.com/flynn/flynn/controller/types"
	"github.com/flynn/flynn/pkg/random"
)

type CliSuite struct {
	Helper
	app     *ct.App
	release *ct.Release
}

var _ = c.Suite(&CliSuite{})

func (s *CliSuite) SetUpSuite(t *c.C) {
	s.app, s.release = s.createApp(t)
}

func (s *CliSuite) TearDownSuite(t *c.C) {
	s.cleanup()
}

func (s *CliSuite) flynn(t *c.C, args ...string) *CmdResult {
	if args[0] != "-a" {
		args = append([]string{"-a", s.app.Name}, args...)
	}
	return flynn(t, "/", args...)
}

func (s *CliSuite) TestApp(t *c.C) {
	name := random.String(30)
	t.Assert(s.flynn(t, "create", name), Outputs, fmt.Sprintf("Created %s\n", name))
	t.Assert(s.flynn(t, "apps"), OutputContains, name)
	// make sure flynn components are listed
	t.Assert(s.flynn(t, "apps"), OutputContains, "router")
	// flynn delete
	t.Assert(s.flynn(t, "-a", name, "delete", "--yes"), Succeeds)
	t.Assert(s.flynn(t, "apps"), c.Not(OutputContains), name)
}

// TODO: share with cli/key.go
func formatKeyID(s string) string {
	buf := make([]byte, 0, len(s)+((len(s)-2)/2))
	for i := range s {
		buf = append(buf, s[i])
		if (i+1)%2 == 0 && i != len(s)-1 {
			buf = append(buf, ':')
		}
	}
	return string(buf)
}

func (s *CliSuite) TestKey(t *c.C) {
	key, err := genSSHKey()
	t.Assert(err, c.IsNil)
	t.Assert(s.flynn(t, "key", "add", key.Pub), Succeeds)

	// calculate fingerprint
	data, err := ioutil.ReadFile(key.Pub)
	t.Assert(err, c.IsNil)
	pubKey, _, _, _, err := ssh.ParseAuthorizedKey(data)
	t.Assert(err, c.IsNil)
	digest := md5.Sum(pubKey.Marshal())
	fingerprint := formatKeyID(hex.EncodeToString(digest[:]))

	t.Assert(s.flynn(t, "key"), OutputContains, fingerprint)
	t.Assert(s.flynn(t, "key", "remove", fingerprint), Succeeds)
	t.Assert(s.flynn(t, "key"), c.Not(OutputContains), fingerprint)
}

func (s *CliSuite) TestPs(t *c.C) {
	stream, err := s.controllerClient(t).StreamJobEvents(s.app.Name, 0)
	if err != nil {
		t.Error(err)
	}
	ps := func() []string {
		out := s.flynn(t, "ps")
		t.Assert(out, Succeeds)
		ary := strings.Split(out.Output, "\n")
		return ary[1 : len(ary)-1]
	}
	// empty formation == empty ps
	t.Assert(ps(), c.HasLen, 0)
	t.Assert(s.flynn(t, "scale", "echoer=3"), Succeeds)
	waitForJobEvents(t, stream.Events, jobEvents{"echoer": {"up": 3}})
	jobs := ps()
	// should return 3 jobs
	t.Assert(jobs, c.HasLen, 3)
	// check job types
	for _, j := range jobs {
		t.Assert(j, Matches, "echoer")
	}
	t.Assert(s.flynn(t, "scale", "echoer=0"), Succeeds)
	waitForJobEvents(t, stream.Events, jobEvents{"echoer": {"down": 3}})
	t.Assert(ps(), c.HasLen, 0)
}

func (s *CliSuite) TestScale(t *c.C) {
	stream, err := s.controllerClient(t).StreamJobEvents(s.app.Name, 0)
	if err != nil {
		t.Error(err)
	}
	t.Assert(s.flynn(t, "scale", "echoer=1"), Succeeds)
	waitForJobEvents(t, stream.Events, jobEvents{"echoer": {"up": 1}})
	// should only start the missing two jobs
	t.Assert(s.flynn(t, "scale", "echoer=3"), Succeeds)
	waitForJobEvents(t, stream.Events, jobEvents{"echoer": {"up": 2}})
	// should stop all jobs
	t.Assert(s.flynn(t, "scale", "echoer=0"), Succeeds)
	waitForJobEvents(t, stream.Events, jobEvents{"echoer": {"down": 3}})
}

func (s *CliSuite) TestEnv(t *c.C) {
	t.Assert(s.flynn(t, "env", "set", "ENV_TEST=var", "SECOND_VAL=2"), Succeeds)
	t.Assert(s.flynn(t, "env"), OutputContains, "ENV_TEST=var\nSECOND_VAL=2")
	t.Assert(s.flynn(t, "env", "get", "ENV_TEST"), Outputs, "var\n")
	// test that containers do contain the ENV var
	t.Assert(s.flynn(t, "run", "-e", "bash", "--", "-c", "echo $ENV_TEST"), Outputs, "var\n")
	t.Assert(s.flynn(t, "env", "unset", "ENV_TEST"), Succeeds)
	t.Assert(s.flynn(t, "run", "-e", "bash", "--", "-c", "echo $ENV_TEST"), Outputs, "\n")
}

func (s *CliSuite) TestKill(t *c.C) {
	stream, err := s.controllerClient(t).StreamJobEvents(s.app.Name, 0)
	if err != nil {
		t.Error(err)
	}
	t.Assert(s.flynn(t, "scale", "echoer=1"), Succeeds)

	_, jobID := waitForJobEvents(t, stream.Events, jobEvents{"echoer": {"up": 1}})
	t.Assert(s.flynn(t, "kill", jobID), Succeeds)
	// detect the job being killed
outer:
	for {
		select {
		case e := <-stream.Events:
			if strings.Contains(jobID, e.JobID) && e.State == "down" {
				break outer
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for job kill event")
		}
	}
	t.Assert(s.flynn(t, "scale", "echoer=0"), Succeeds)
	waitForJobEvents(t, stream.Events, jobEvents{"echoer": {"down": 1}})
}

func (s *CliSuite) TestRoute(t *c.C) {
	// flynn route add http
	route := random.String(32) + ".dev"
	newRoute := s.flynn(t, "route", "add", "http", route)
	t.Assert(newRoute, Succeeds)
	routeID := strings.TrimSpace(newRoute.Output)
	t.Assert(s.flynn(t, "route"), OutputContains, routeID)

	// flynn route remove
	t.Assert(s.flynn(t, "route", "remove", routeID), Succeeds)
	t.Assert(s.flynn(t, "route"), c.Not(OutputContains), routeID)

	// flynn route add tcp
	tcpRoute := s.flynn(t, "route", "add", "tcp")
	t.Assert(tcpRoute, Succeeds)
	routeID = strings.Split(tcpRoute.Output, " ")[0]
	t.Assert(s.flynn(t, "route"), OutputContains, routeID)

	// flynn route remove
	t.Assert(s.flynn(t, "route", "remove", routeID), Succeeds)
	t.Assert(s.flynn(t, "route"), c.Not(OutputContains), routeID)
}

func (s *CliSuite) TestProvider(t *c.C) {
	t.Assert(s.flynn(t, "provider"), OutputContains, "postgres")
}

func (s *CliSuite) TestResource(t *c.C) {
	t.Assert(s.flynn(t, "resource", "add", "postgres").Output, Matches, `Created resource \w+ and release \w+.`)
	res, err := s.controllerClient(t).AppResourceList(s.app.Name)
	t.Assert(err, c.IsNil)
	t.Assert(res, c.HasLen, 1)
}

func (s *CliSuite) TestLog(t *c.C) {
	stream, err := s.controllerClient(t).StreamJobEvents(s.app.Name, 0)
	if err != nil {
		t.Error(err)
	}
	t.Assert(s.flynn(t, "scale", "printer=1"), Succeeds)
	_, jobID := waitForJobEvents(t, stream.Events, jobEvents{"printer": {"up": 1}})

	t.Assert(s.flynn(t, "log", jobID), OutputContains, "I like to print")

	t.Assert(s.flynn(t, "scale", "printer=0"), Succeeds)
	waitForJobEvents(t, stream.Events, jobEvents{"printer": {"down": 1}})
}

func (s *CliSuite) TestCluster(t *c.C) {
	// cluster add
	t.Assert(s.flynn(t, "cluster", "add", "-g", "test.example.com:2222", "-p", "KGCENkp53YF5OvOKkZIry71+czFRkSw2ZdMszZ/0ljs=", "test", "https://controller.test.example.com", "e09dc5301d72be755a3d666f617c4600"), Succeeds)
	t.Assert(s.flynn(t, "cluster"), OutputContains, "test")
	// overwriting should not work
	t.Assert(s.flynn(t, "cluster", "add", "test", "foo", "bar"), c.Not(Succeeds))
	t.Assert(s.flynn(t, "cluster"), OutputContains, "test")
	// cluster remove
	t.Assert(s.flynn(t, "cluster", "remove", "test"), Succeeds)
	t.Assert(s.flynn(t, "cluster"), c.Not(OutputContains), "test")
}
