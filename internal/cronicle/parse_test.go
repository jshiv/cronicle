package cronicle_test

import (
	"encoding/json"
	"os"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	"github.com/jshiv/cronicle/internal/cronicle"

	hcl "github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclparse"
	"github.com/hashicorp/hcl/v2/hclsimple"

	"strings"

	"github.com/zclconf/go-cty/cty"
)

var _ = Describe("Parse", func() {

	It("cronicle.CommandEvalContext should contain date, datetime, and timestamp as an argument", func() {

		expected := hcl.EvalContext{
			Variables: map[string]cty.Value{
				"date":      cty.StringVal("${date}"),
				"datetime":  cty.StringVal("${datetime}"),
				"timestamp": cty.StringVal("${timestamp}"),
			},
		}
		Expect(cronicle.CommandEvalContext).To(Equal(expected))
	})

	It("cronicle.Config should be parsable given a date argument", func() {

		var conf cronicle.Config
		err := hclsimple.DecodeFile("./test/config.hcl", &cronicle.CommandEvalContext, &conf)
		Expect(err).To(BeNil())
		Expect(conf.Schedules[0].Tasks[0].Command).To(Equal([]string{"/bin/echo", "Hello World", "--date=${date}"}))
	})

	It("cronicle.Config should be parsable given a date argument: ${date}", func() {

		conf := cronicle.Default()
		conf.Schedules[0].Tasks[0].Command = []string{"/bin/echo", "Hello World --date=${date}"}

		f := conf.Hcl()

		test := strings.Contains(string(f.Bytes), `["/bin/echo", "Hello World --date=${date}"]`)
		Expect(test).To(Equal(true))
	})

	It("cronicle.MarshallHcl should be write a file given a date argument: ${date}", func() {

		conf := cronicle.Default()
		conf.Schedules[0].Tasks[0].Command = []string{"/bin/echo", "Hello World --date=${date}"}

		p := cronicle.MarshallHcl(conf, "./test/test.hcl")

		var c cronicle.Config
		err := hclsimple.DecodeFile(p, &cronicle.CommandEvalContext, &c)
		Expect(err).To(BeNil())
		Expect(conf).To(Equal(c))
		os.RemoveAll(p)
	})

	It("cronicle.ParseFile raise diags if a file is malformatted", func() {

		parser := hclparse.NewParser()
		conf, diags := cronicle.ParseFile("./test/bad.hcl", parser)
		Expect(diags[0].Detail).To(Equal("A block definition must have block content delimited by \"{\" and \"}\", starting on the same line as the block header."))
		Expect(conf).To(BeNil())

	})

	It("schedule.JSON should return []byte", func() {
		conf := cronicle.Default()
		schedule := conf.Schedules[0]
		// schedule.Now = time.Now().In(time.Local)
		s := `{"Name":"foo","Cron":"@every 5s","Timezone":"","StartDate":"","EndDate":"","Repo":null,"Tasks":[{"Name":"bar","Command":["/bin/echo","Hello World --date=${date}"],"Depends":null,"Repo":null,"Retry":null,"Env":null,"Path":"","CronicleRepo":null,"CroniclePath":"","Git":{"Worktree":null,"Repository":null,"Head":null,"Hash":null,"Commit":null,"ReferenceName":""},"ScheduleName":""}],"Now":"0001-01-01T00:00:00Z","CronicleRepo":null}`

		Expect(schedule.JSON()).To(Equal([]byte(s)))
	})

	It("json.Unmarshal(schedule.JSON) should equal schedule", func() {
		conf := cronicle.Default()
		schedule := conf.Schedules[0]
		// schedule.Now = time.Now().In(time.Local)
		j := schedule.JSON()
		var sched cronicle.Schedule
		err := json.Unmarshal(j, &sched)
		Expect(err).To(BeNil())
		Expect(sched).To(Equal(schedule))
	})

})
