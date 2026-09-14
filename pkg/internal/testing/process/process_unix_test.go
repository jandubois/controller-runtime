//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris || zos

/*
Copyright 2026 The Kubernetes Authors.

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

package process_test

import (
	"bufio"
	"io"
	"strconv"
	"strings"
	"syscall"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/ghttp"
	. "sigs.k8s.io/controller-runtime/pkg/internal/testing/process"
)

// Only unix puts the process in its own group and signals it. On Windows,
// Stop kills just the process, which exits before a 1ns StopTimeout expires.
var _ = Describe("Stop method", func() {
	var (
		server       *ghttp.Server
		processState *State
	)
	BeforeEach(func() {
		server = ghttp.NewServer()
		processState = newHealthyState(server)
	})

	AfterEach(func() {
		server.Close()
	})

	Context("when the command cannot be stopped", func() {
		It("returns a timeout error", func() {
			Expect(processState.Start(nil, nil)).To(Succeed())
			processState.StopTimeout = 1 * time.Nanosecond // much shorter than the sleep in the script

			Expect(processState.Stop()).To(MatchError(ContainSubstring("timeout")))
		})
	})

	Context("when the process spawns children", func() {
		It("stops the full process group including all its children", func() {
			pr, pw := io.Pipe()
			defer pr.Close()
			defer pw.Close()

			processState.Args = []string{
				"-c",
				"trap 'if [ -n \"${child_pid}\" ]; then wait \"${child_pid}\"; fi; exit 0' TERM INT; " +
					"sleep 30 & child_pid=$!; echo ${child_pid}; wait \"${child_pid}\"",
			}
			processState.StopTimeout = 10 * time.Second

			childPIDChan := make(chan int, 1)
			go func() {
				scanner := bufio.NewScanner(pr)
				if scanner.Scan() {
					if pid, err := strconv.Atoi(strings.TrimSpace(scanner.Text())); err == nil {
						childPIDChan <- pid
					}
				}
			}()

			Expect(processState.Start(pw, nil)).To(Succeed())
			expectedProcessGroupID := processState.Cmd.Process.Pid

			// wait until process started and we've captured the childPID
			var childPID int
			Eventually(childPIDChan, 5*time.Second).Should(Receive(&childPID))
			DeferCleanup(func() {
				if processState.Cmd != nil && processState.Cmd.Process != nil {
					_ = syscall.Kill(-processState.Cmd.Process.Pid, syscall.SIGKILL)
				}
			})

			// call stop on the process and expect child pid lookups to eventually
			// tell us that no process can be found
			Expect(processState.Stop()).To(Succeed())
			Eventually(func() bool {
				pgid, err := syscall.Getpgid(childPID)
				if err == syscall.ESRCH {
					return true
				}
				if err != nil {
					return false
				}

				// if the pid was re-used by another process, it won't be in the original process group
				return pgid != expectedProcessGroupID
			}, 5*time.Second).Should(BeTrue())
		})
	})
})
