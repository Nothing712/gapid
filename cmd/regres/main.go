// Copyright (C) 2018 Google Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Regress is a tool to display build and runtime statistics over a range of
// changelists.
package main

import (
	"context"
	"flag"
	"fmt"
	"io/ioutil"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/google/gapid/core/app"
	"github.com/google/gapid/core/git"
	"github.com/google/gapid/core/log"
	"github.com/google/gapid/core/os/shell"
)

var (
	root     = flag.String("root", "", "Path to the root GAPID source directory")
	verbose  = flag.Bool("verbose", false, "Verbose logging")
	incBuild = flag.Bool("inc", true, "Time incremental builds")
	optimize = flag.Bool("optimize", false, "Build using '-c opt'")
	pkg      = flag.String("pkg", "", "Partial name of a package name to capture")
	output   = flag.String("out", "", "The results output file. Empty writes to stdout")
	atSHA    = flag.String("at", "", "The SHA or branch of the first changelist to profile")
	count    = flag.Int("count", 2, "The number of changelists to profile since HEAD")
)

func main() {
	app.ShortHelp = "Regress is a tool to perform performance measurments over a range of CLs."
	app.Run(run)
}

type stats struct {
	SHA                  string
	BuildTime            float64  // in seconds
	IncrementalBuildTime float64  // in seconds
	FileSizes            struct { // in bytes
		LibGAPII                   int64
		LibVkLayerVirtualSwapchain int64
		GAPIDAarch64APK            int64
		GAPIDArmeabi64APK          int64
		GAPIDX86APK                int64
		GAPID                      int64
		GAPIR                      int64
		GAPIS                      int64
		GAPIT                      int64
	}
	CaptureStats struct {
		Frames    int
		DrawCalls int
		Commands  int
	}
}

func run(ctx context.Context) error {
	if *root == "" {
		wd, err := os.Getwd()
		if err != nil {
			return err
		}
		*root = wd
	}

	g, err := git.New(*root)
	if err != nil {
		return err
	}
	s, err := g.Status(ctx)
	if err != nil {
		return err
	}
	if !s.Clean() {
		return fmt.Errorf("Local changes found. Please submit any changes and run again")
	}

	branch, err := g.CurrentBranch(ctx)
	if err != nil {
		return err
	}

	defer g.CheckoutBranch(ctx, branch)

	cls, err := g.LogFrom(ctx, *atSHA, *count)
	if err != nil {
		return err
	}

	rnd := rand.New(rand.NewSource(time.Now().Unix()))

	res := []stats{}
	for i := range cls {
		i := len(cls) - 1 - i
		cl := cls[i]
		sha := cl.SHA.String()[:6]

		r := stats{SHA: sha}

		log.I(ctx, "HEAD~%.2d: Building at %v: %v", i, sha, cl.Subject)
		if err := g.Checkout(ctx, cl.SHA); err != nil {
			return err
		}

		_, err := build(ctx)
		if err != nil {
			continue
		}

		// Gather file size build stats
		pkgDir := filepath.Join(*root, "bazel-bin", "pkg")
		for _, f := range []struct {
			path string
			size *int64
		}{
			{filepath.Join(pkgDir, "lib", dllExt("libgapii")), &r.FileSizes.LibGAPII},
			{filepath.Join(pkgDir, "lib", dllExt("libVkLayer_VirtualSwapchain")), &r.FileSizes.LibVkLayerVirtualSwapchain},
			{filepath.Join(pkgDir, "gapid-aarch64.apk"), &r.FileSizes.GAPIDAarch64APK},
			{filepath.Join(pkgDir, "gapid-armeabi.apk"), &r.FileSizes.GAPIDArmeabi64APK},
			{filepath.Join(pkgDir, "gapid-x86.apk"), &r.FileSizes.GAPIDX86APK},
			{filepath.Join(pkgDir, exeExt("gapid")), &r.FileSizes.GAPID},
			{filepath.Join(pkgDir, exeExt("gapir")), &r.FileSizes.GAPIR},
			{filepath.Join(pkgDir, exeExt("gapis")), &r.FileSizes.GAPIS},
			{filepath.Join(pkgDir, exeExt("gapit")), &r.FileSizes.GAPIT},
		} {
			fi, err := os.Stat(f.path)
			if err != nil {
				log.W(ctx, "Couldn't stat file '%v': %v", f.path, err)
				continue
			}
			*f.size = fi.Size()
		}

		// Gather capture stats
		if *pkg != "" {
			file, err := trace(ctx)
			if err != nil {
				log.W(ctx, "Couldn't capture trace: %v", err)
				continue
			}
			defer os.Remove(file)
			frames, draws, cmds, err := captureStats(ctx, file)
			if err != nil {
				continue
			}
			r.CaptureStats.Frames = frames
			r.CaptureStats.DrawCalls = draws
			r.CaptureStats.Commands = cmds
		}

		// Gather incremental build stats
		if *incBuild {
			if err := withTouchedGLES(ctx, rnd, func() error {
				log.I(ctx, "HEAD~%.2d: Building incremental change at %v: %v", i, sha, cl.Subject)
				if duration, err := build(ctx); err == nil {
					r.IncrementalBuildTime = duration.Seconds()
				}
				return nil
			}); err != nil {
				continue
			}
		}

		res = append(res, r)
	}

	fmt.Printf("-----------------------\n")

	w := tabwriter.NewWriter(os.Stdout, 1, 4, 0, ' ', 0)
	defer w.Flush()

	fmt.Fprint(w, "sha")
	if *incBuild {
		fmt.Fprint(w, "\t | incremental_build_time")
	}
	if *pkg != "" {
		fmt.Fprint(w, "\t | commands")
		fmt.Fprint(w, "\t | draws")
		fmt.Fprint(w, "\t | frames")
	}
	fmt.Fprint(w, "\t | lib_gapii")
	fmt.Fprint(w, "\t | lib_swapchain")
	fmt.Fprint(w, "\t | aarch64.apk")
	fmt.Fprint(w, "\t | armeabi64.apk")
	fmt.Fprint(w, "\t | x86.apk")
	fmt.Fprint(w, "\t | gapid")
	fmt.Fprint(w, "\t | gapir")
	fmt.Fprint(w, "\t | gapis")
	fmt.Fprint(w, "\t | gapit\n")
	for _, r := range res {
		fmt.Fprintf(w, "%v,", r.SHA)
		if *incBuild {
			fmt.Fprintf(w, "\t   %v,", r.IncrementalBuildTime)
		}
		if *pkg != "" {
			fmt.Fprintf(w, "\t   %v,", r.CaptureStats.Commands)
			fmt.Fprintf(w, "\t   %v,", r.CaptureStats.DrawCalls)
			fmt.Fprintf(w, "\t   %v,", r.CaptureStats.Frames)
		}
		fmt.Fprintf(w, "\t   %v,", r.FileSizes.LibGAPII)
		fmt.Fprintf(w, "\t   %v,", r.FileSizes.LibVkLayerVirtualSwapchain)
		fmt.Fprintf(w, "\t   %v,", r.FileSizes.GAPIDAarch64APK)
		fmt.Fprintf(w, "\t   %v,", r.FileSizes.GAPIDArmeabi64APK)
		fmt.Fprintf(w, "\t   %v,", r.FileSizes.GAPIDX86APK)
		fmt.Fprintf(w, "\t   %v,", r.FileSizes.GAPID)
		fmt.Fprintf(w, "\t   %v,", r.FileSizes.GAPIR)
		fmt.Fprintf(w, "\t   %v,", r.FileSizes.GAPIS)
		fmt.Fprintf(w, "\t   %v", r.FileSizes.GAPIT)
		fmt.Fprintf(w, "\n")
	}
	return nil
}

func withTouchedGLES(ctx context.Context, r *rand.Rand, f func() error) error {
	glesAPIPath := filepath.Join(*root, "gapis", "api", "gles", "gles.api")
	fi, err := os.Stat(glesAPIPath)
	if err != nil {
		return err
	}
	glesAPI, err := ioutil.ReadFile(glesAPIPath)
	if err != nil {
		return err
	}
	modGlesAPI := []byte(fmt.Sprintf("%v\ncmd void fake_cmd_%d() {}\n", string(glesAPI), r.Int()))
	ioutil.WriteFile(glesAPIPath, modGlesAPI, fi.Mode().Perm())
	defer ioutil.WriteFile(glesAPIPath, glesAPI, fi.Mode().Perm())
	return f()
}

func build(ctx context.Context) (time.Duration, error) {
	args := []string{"build"}
	if *optimize {
		args = append(args, "-c", "opt")
	}
	args = append(args, "pkg")
	cmd := shell.Cmd{
		Name:      "bazel",
		Args:      args,
		Verbosity: *verbose,
		Dir:       *root,
	}
	start := time.Now()
	_, err := cmd.Call(ctx)
	return time.Since(start), err
}

func dllExt(n string) string {
	switch runtime.GOOS {
	case "windows":
		return n + ".dll"
	case "darwin":
		return n + ".dylib"
	default:
		return n + ".so"
	}
}

func exeExt(n string) string {
	switch runtime.GOOS {
	case "windows":
		return n + ".exe"
	default:
		return n
	}
}

func gapitPath() string { return filepath.Join(*root, "bazel-bin", "pkg", exeExt("gapit")) }

func trace(ctx context.Context) (string, error) {
	file := filepath.Join(os.TempDir(), "gapid-regres.gfxtrace")
	cmd := shell.Cmd{
		Name:      gapitPath(),
		Args:      []string{"--log-style", "raw", "trace", "--for", "60s", "--out", file, *pkg},
		Verbosity: *verbose,
	}
	_, err := cmd.Call(ctx)
	if err != nil {
		os.Remove(file)
		return "", err
	}
	return file, err
}

func captureStats(ctx context.Context, file string) (numFrames, numDraws, numCmds int, err error) {
	cmd := shell.Cmd{
		Name:      gapitPath(),
		Args:      []string{"--log-style", "raw", "--log-level", "error", "stats", file},
		Verbosity: *verbose,
	}
	stdout, err := cmd.Call(ctx)
	if err != nil {
		return 0, 0, 0, nil
	}
	re := regexp.MustCompile(`([a-zA-Z]+):\s+([0-9]+)`)
	for _, matches := range re.FindAllStringSubmatch(stdout, -1) {
		if len(matches) != 3 {
			continue
		}
		n, err := strconv.Atoi(matches[2])
		if err != nil {
			continue
		}
		switch matches[1] {
		case "Frames":
			numFrames = n
		case "Draws":
			numDraws = n
		case "Commands":
			numCmds = n
		}
	}
	return
}