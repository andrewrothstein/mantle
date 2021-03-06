// Copyright 2015 CoreOS, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package kola

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/coreos/go-semver/semver"
	"github.com/coreos/pkg/capnslog"

	"github.com/coreos/mantle/harness"
	"github.com/coreos/mantle/kola/cluster"
	"github.com/coreos/mantle/kola/register"
	"github.com/coreos/mantle/platform"
	awsapi "github.com/coreos/mantle/platform/api/aws"
	esxapi "github.com/coreos/mantle/platform/api/esx"
	gcloudapi "github.com/coreos/mantle/platform/api/gcloud"
	packetapi "github.com/coreos/mantle/platform/api/packet"
	"github.com/coreos/mantle/platform/machine/aws"
	"github.com/coreos/mantle/platform/machine/esx"
	"github.com/coreos/mantle/platform/machine/gcloud"
	"github.com/coreos/mantle/platform/machine/packet"
	"github.com/coreos/mantle/platform/machine/qemu"
	"github.com/coreos/mantle/system"
)

var (
	plog = capnslog.NewPackageLogger("github.com/coreos/mantle", "kola")

	Options       = platform.Options{}
	QEMUOptions   = qemu.Options{Options: &Options}      // glue to set platform options from main
	GCEOptions    = gcloudapi.Options{Options: &Options} // glue to set platform options from main
	AWSOptions    = awsapi.Options{Options: &Options}    // glue to set platform options from main
	PacketOptions = packetapi.Options{Options: &Options} // glue to set platform options from main
	ESXOptions    = esxapi.Options{Options: &Options}    // glue to set platform options from main

	TestParallelism int    //glue var to set test parallelism from main
	TAPFile         string // if not "", write TAP results here

	consoleChecks = []struct {
		desc     string
		match    *regexp.Regexp
		skipFlag *register.Flag
	}{
		{
			desc:     "emergency shell",
			match:    regexp.MustCompile("Press Enter for emergency shell|Starting Emergency Shell|You are in emergency mode"),
			skipFlag: &[]register.Flag{register.NoEmergencyShellCheck}[0],
		},
		{
			desc:  "kernel panic",
			match: regexp.MustCompile("Kernel panic - not syncing"),
		},
		{
			desc:  "kernel oops",
			match: regexp.MustCompile("Oops:"),
		},
		{
			desc:  "Go panic",
			match: regexp.MustCompile("panic\\("),
		},
		{
			desc:  "segfault",
			match: regexp.MustCompile("SEGV"),
		},
		{
			desc:  "core dump",
			match: regexp.MustCompile("[Cc]ore dump"),
		},
	}
)

// NativeRunner is a closure passed to all kola test functions and used
// to run native go functions directly on kola machines. It is necessary
// glue until kola does introspection.
type NativeRunner func(funcName string, m platform.Machine) error

func NewCluster(pltfrm string, rconf *platform.RuntimeConfig) (cluster platform.Cluster, err error) {
	switch pltfrm {
	case "qemu":
		cluster, err = qemu.NewCluster(&QEMUOptions, rconf)
	case "gce":
		cluster, err = gcloud.NewCluster(&GCEOptions, rconf)
	case "aws":
		cluster, err = aws.NewCluster(&AWSOptions, rconf)
	case "packet":
		cluster, err = packet.NewCluster(&PacketOptions, rconf)
	case "esx":
		cluster, err = esx.NewCluster(&ESXOptions, rconf)
	default:
		err = fmt.Errorf("invalid platform %q", pltfrm)
	}
	return
}

func filterTests(tests map[string]*register.Test, pattern, platform string, version semver.Version) (map[string]*register.Test, error) {
	r := make(map[string]*register.Test)

	for name, t := range tests {
		match, err := filepath.Match(pattern, t.Name)
		if err != nil {
			return nil, err
		}
		if !match {
			continue
		}

		// Check the test's min and end versions when running more then one test
		if t.Name != pattern && versionOutsideRange(version, t.MinVersion, t.EndVersion) {
			continue
		}

		allowed := true
		for _, p := range t.Platforms {
			if p == platform {
				allowed = true
				break
			} else {
				allowed = false
			}
		}
		for _, p := range t.ExcludePlatforms {
			if p == platform {
				allowed = false
			}
		}
		if !allowed {
			continue
		}

		arch := architecture(platform)
		for _, a := range t.Architectures {
			if a == arch {
				allowed = true
				break
			} else {
				allowed = false
			}
		}
		if !allowed {
			continue
		}

		r[name] = t
	}

	return r, nil
}

// versionOutsideRange checks to see if version is outside [min, end). If end
// is a zero value, it is ignored and there is no upper bound. If version is a
// zero value, the bounds are ignored.
func versionOutsideRange(version, minVersion, endVersion semver.Version) bool {
	if version == (semver.Version{}) {
		return false
	}

	if version.LessThan(minVersion) {
		return true
	}

	if (endVersion != semver.Version{}) && !version.LessThan(endVersion) {
		return true
	}

	return false
}

// RunTests is a harness for running multiple tests in parallel. Filters
// tests based on a glob pattern and by platform. Has access to all
// tests either registered in this package or by imported packages that
// register tests in their init() function.
// outputDir is where various test logs and data will be written for
// analysis after the test run. If it already exists it will be erased!
func RunTests(pattern, pltfrm, outputDir string) error {
	// Avoid incurring cost of starting machine in getClusterSemver when
	// either:
	// 1) none of the selected tests care about the version
	// 2) glob is an exact match which means minVersion will be ignored
	//    either way
	tests, err := filterTests(register.Tests, pattern, pltfrm, semver.Version{})
	if err != nil {
		plog.Fatal(err)
	}

	skipGetVersion := true
	for name, t := range tests {
		if name != pattern && (t.MinVersion != semver.Version{} || t.EndVersion != semver.Version{}) {
			skipGetVersion = false
			break
		}
	}

	if !skipGetVersion {
		version, err := getClusterSemver(pltfrm, outputDir)
		if err != nil {
			plog.Fatal(err)
		}

		// one more filter pass now that we know real version
		tests, err = filterTests(tests, pattern, pltfrm, *version)
		if err != nil {
			plog.Fatal(err)
		}
	}

	opts := harness.Options{
		OutputDir: outputDir,
		Parallel:  TestParallelism,
		Verbose:   true,
	}
	var htests harness.Tests
	for _, test := range tests {
		test := test // for the closure
		run := func(h *harness.H) {
			runTest(h, test, pltfrm)
		}
		htests.Add(test.Name, run)
	}

	suite := harness.NewSuite(opts, htests)
	err = suite.Run()

	if TAPFile != "" {
		src := filepath.Join(outputDir, "test.tap")
		if err2 := system.CopyRegularFile(src, TAPFile); err == nil && err2 != nil {
			err = err2
		}
	}

	if err != nil {
		fmt.Printf("FAIL, output in %v\n", outputDir)
	} else {
		fmt.Printf("PASS, output in %v\n", outputDir)
	}

	return err
}

// getClusterSemVer returns the CoreOS semantic version via starting a
// machine and checking
func getClusterSemver(pltfrm, outputDir string) (*semver.Version, error) {
	var err error

	testDir := filepath.Join(outputDir, "get_cluster_semver")
	if err := os.MkdirAll(testDir, 0777); err != nil {
		return nil, err
	}

	cluster, err := NewCluster(pltfrm, &platform.RuntimeConfig{
		OutputDir: testDir,
	})
	if err != nil {
		return nil, fmt.Errorf("creating cluster for semver check: %v", err)
	}
	defer func() {
		if err := cluster.Destroy(); err != nil {
			plog.Errorf("cluster.Destroy(): %v", err)
		}
	}()

	m, err := cluster.NewMachine(nil)
	if err != nil {
		return nil, fmt.Errorf("creating new machine for semver check: %v", err)
	}

	out, err := m.SSH("grep ^VERSION_ID= /etc/os-release")
	if err != nil {
		return nil, fmt.Errorf("parsing /etc/os-release: %v", err)
	}

	version, err := semver.NewVersion(strings.Split(string(out), "=")[1])
	if err != nil {
		return nil, fmt.Errorf("parsing os-release semver: %v", err)
	}

	return version, nil
}

// runTest is a harness for running a single test.
// outputDir is where various test logs and data will be written for
// analysis after the test run. It should already exist.
func runTest(h *harness.H, t *register.Test, pltfrm string) {
	h.Parallel()

	// don't go too fast, in case we're talking to a rate limiting api like AWS EC2.
	// FIXME(marineam): API requests must do their own
	// backoff due to rate limiting, this is unreliable.
	max := int64(2 * time.Second)
	splay := time.Duration(rand.Int63n(max))
	time.Sleep(splay)

	rconf := &platform.RuntimeConfig{
		OutputDir:          h.OutputDir(),
		NoSSHKeyInUserData: t.HasFlag(register.NoSSHKeyInUserData),
		NoSSHKeyInMetadata: t.HasFlag(register.NoSSHKeyInMetadata),
		NoEnableSelinux:    t.HasFlag(register.NoEnableSelinux),
	}
	c, err := NewCluster(pltfrm, rconf)
	if err != nil {
		h.Fatalf("Cluster failed: %v", err)
	}
	defer func() {
		if err := c.Destroy(); err != nil {
			plog.Errorf("cluster.Destroy(): %v", err)
		}
		checkConsole(h, t, c)
	}()

	if t.ClusterSize > 0 {
		url, err := c.GetDiscoveryURL(t.ClusterSize)
		if err != nil {
			h.Fatalf("Failed to create discovery endpoint: %v", err)
		}

		userdata := t.UserData
		if userdata != nil {
			userdata = userdata.Subst("$discovery", url)
		}
		if _, err := platform.NewMachines(c, userdata, t.ClusterSize); err != nil {
			h.Fatalf("Cluster failed starting machines: %v", err)
		}
	}

	// pass along all registered native functions
	var names []string
	for k := range t.NativeFuncs {
		names = append(names, k)
	}

	// Cluster -> TestCluster
	tcluster := cluster.TestCluster{
		H:           h,
		Cluster:     c,
		NativeFuncs: names,
	}

	// drop kolet binary on machines
	if t.NativeFuncs != nil {
		scpKolet(tcluster, architecture(pltfrm))
	}

	defer func() {
		// give some time for the remote journal to be flushed so it can be read
		// before we run the deferred machine destruction
		time.Sleep(2 * time.Second)
	}()

	// run test
	t.Run(tcluster)
}

// architecture returns the machine architecture of the given platform.
func architecture(pltfrm string) string {
	nativeArch := "amd64"
	if pltfrm == "qemu" && QEMUOptions.Board != "" {
		nativeArch = boardToArch(QEMUOptions.Board)
	}
	if pltfrm == "packet" && PacketOptions.Board != "" {
		nativeArch = boardToArch(PacketOptions.Board)
	}
	return nativeArch
}

// returns the arch part of an sdk board name
func boardToArch(board string) string {
	return strings.SplitN(board, "-", 2)[0]
}

// scpKolet searches for a kolet binary and copies it to the machine.
func scpKolet(c cluster.TestCluster, mArch string) {
	for _, d := range []string{
		".",
		filepath.Dir(os.Args[0]),
		filepath.Join(filepath.Dir(os.Args[0]), mArch),
		filepath.Join("/usr/lib/kola", mArch),
	} {
		kolet := filepath.Join(d, "kolet")
		if _, err := os.Stat(kolet); err == nil {
			if err := c.DropFile(kolet); err != nil {
				c.Fatalf("dropping kolet binary: %v", err)
			}
			return
		}
	}
	c.Fatalf("Unable to locate kolet binary for %s", mArch)
}

func checkConsole(h *harness.H, t *register.Test, c platform.Cluster) {
	for id, output := range c.ConsoleOutput() {
		for _, check := range consoleChecks {
			if check.skipFlag != nil {
				if t.HasFlag(*check.skipFlag) {
					continue
				}
			}
			if check.match.Find([]byte(output)) != nil {
				h.Errorf("Found %s on machine %s console", check.desc, id)
			}
		}
	}
}

func SetupOutputDir(outputDir, platform string) (string, error) {
	defaulted := outputDir == ""
	defaultBaseDirName := "_kola_temp"
	defaultDirName := fmt.Sprintf("%s-%s-%d", platform, time.Now().Format("2006-01-02-1504"), os.Getpid())

	if defaulted {
		if _, err := os.Stat(defaultBaseDirName); os.IsNotExist(err) {
			if err := os.Mkdir(defaultBaseDirName, 0777); err != nil {
				return "", err
			}
		}
		outputDir = filepath.Join(defaultBaseDirName, defaultDirName)
	}

	outputDir, err := harness.CleanOutputDir(outputDir)
	if err != nil {
		return "", err
	}

	if defaulted {
		linkPath := filepath.Join(defaultBaseDirName, platform+"-latest")
		st, err := os.Lstat(linkPath)
		if err == nil {
			if (st.Mode() & os.ModeType) != os.ModeSymlink {
				return "", fmt.Errorf("%v exists and is not a symlink", linkPath)
			}
			if err := os.Remove(linkPath); err != nil {
				return "", err
			}
		} else if !os.IsNotExist(err) {
			return "", err
		}
		if err := os.Symlink(defaultDirName, linkPath); err != nil {
			return "", err
		}
	}

	return outputDir, nil
}
