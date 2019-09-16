package config_test

import (
	"fmt"

	"github.com/hashicorp/hcl2/gohcl"
	"github.com/hashicorp/hcl2/hclwrite"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	"github.com/jshiv/cronicle/internal/config"
)

var _ = Describe("Config", func() {

	It("Should be configurable from hcl", func() {
		// not testing anything, just an informative dummy
		testConfig := config.Config{
			Version: "0.1",
			Git:     "github.com/myname/schedule1",
			Schedules: []config.Schedule{
				{
					Name:      "My-Schedule",
					Cron:      "@hourly",
					StartDate: "2019-09-10",
					EndDate:   "2019-09-12",
					Owner: &config.Owner{
						Name:  "bob",
						Email: "bob@email.com",
					},
					Tasks: []config.Task{
						{
							Name: "task1",
							Owner: &config.Owner{
								Name:  "bobby",
								Email: "bobby@email.com",
							},
						},
						{
							Name:    "task2",
							Command: []string{"/bin/bash", "job.sh"},
							Depends: []string{"task1"},
							Repo:    "github.com/myname/schedulerepo1",
						},
					},
				},
			},
		}

		f := hclwrite.NewEmptyFile()
		gohcl.EncodeIntoBody(&testConfig, f.Body())
		fmt.Printf(string(f.Bytes()))
		Expect(string(f.Bytes())).ToNot(BeNil())
	})
})
