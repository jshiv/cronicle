package cronicle

import (
	"time"

	"github.com/hashicorp/terraform/dag"
	"github.com/hashicorp/terraform/tfdiags"
	log "github.com/sirupsen/logrus"
)

//TaskGraph produces AcyclicGraph of schedule.Tasks where edges are
//connected by task.Name and task.Depends
func (schedule *Schedule) taskGraph() dag.AcyclicGraph {
	//TODO: Add error case where depName does not exist in schedule.Tasks[i.e. miss spelled]
	var g dag.AcyclicGraph
	var edges []dag.Edge
	for _, task := range schedule.Tasks {
		g.Add(task.Name)
		for _, depName := range task.Depends {
			edges = append(edges, dag.BasicEdge(task.Name, depName))
		}
	}

	for _, edge := range edges {
		g.Connect(edge)
	}

	return g
}

// ExecuteTasks handels the execution of all tasks in a given schedule.
// The execution walks over a DAG[Directed Acyclic Graph] to determine
// execution order, which will default to parallel unless task.depends is
// specified.
func (schedule Schedule) ExecuteTasks() {
	var now time.Time
	if (schedule.Now == time.Time{}) {
		now = time.Now().In(time.Local)
	} else {
		now = schedule.Now
	}

	taskMap := schedule.TaskMap()
	taskGraph := schedule.taskGraph()
	graphString := taskGraph.StringWithNodeTypes()
	log.WithFields(log.Fields{
		"schedule": schedule.Name,
		"clock":    now.Format(time.Kitchen),
		"date":     now.Format(time.RFC850),
	}).Info(graphString)
	err := taskGraph.Walk(func(v dag.Vertex) tfdiags.Diagnostics {
		var diags tfdiags.Diagnostics
		taskName := dag.VertexName(v)
		task := taskMap[taskName]
		_, err := task.Execute(now)

		if err != nil {
			diags = diags.Append(err)
			return diags
		}

		return diags
	})

	if err != nil {
		log.Error(err.Err())
	}

}
