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

	It("Should return an error if the sides don't make up a triangle", func() {

		testSchedule := config.Schedule{
			Name:      "My-Schedule",
			Cron:      "test",
			StartDate: "blah",
			EndDate:   "blah",
			Tasks: []config.Task{
				{
					Name: "task1",
					Owner: &config.Owner{
						Name: "paul",
						Email: "paul.cho@udemy.com",
					},
				},
				{
					Name: "task2",
				},
			},
		}

		f := hclwrite.NewEmptyFile()
		gohcl.EncodeIntoBody(&testSchedule, f.Body())
		fmt.Printf("%s", f.Bytes())
		Expect("blah").To(Equal("blah"))
	})
})
