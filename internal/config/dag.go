package config

import (
	"time"

	"github.com/hashicorp/terraform/dag"
	"github.com/hashicorp/terraform/tfdiags"
	log "github.com/sirupsen/logrus"
)

//TaskGraph produces AcyclicGraph of schedule.Tasks where edges are
//connected by task.Name and task.Depends
func (schedule *Schedule) taskGraph() dag.AcyclicGraph {
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
		"time":     now,
	}).Info(graphString)
	err := taskGraph.Walk(func(v dag.Vertex) tfdiags.Diagnostics {
		var diags tfdiags.Diagnostics
		taskName := dag.VertexName(v)
		task := taskMap[taskName]
		r, err := task.Execute(now)
		if err != nil {
			diags = diags.Append(err)
			return diags
		}
		task.Log(r)

		return diags
	})

	if err != nil {
		log.Error(err)
	}

}

// type Task struct {
// 	Name    string
// 	Depends []string
// }

// func exec(name string) {
// 	fmt.Println("Starting Task: ", name)
// 	time.Sleep(time.Second * 4)
// 	fmt.Println("End Task: ", name)
// }

// func main() {

// 	var g dag.AcyclicGraph

// 	var tasks []Task

// 	task1 := Task{Name: "task1"}
// 	task2 := Task{Name: "task2", Depends: []string{"task1"}}

// 	tasks = append(tasks, task1)
// 	tasks = append(tasks, task2)

// 	var edges []dag.Edge
// 	for _, task := range tasks {
// 		fmt.Println(task.Name)
// 		fmt.Println(task.Depends)
// 		g.Add(task.Name)
// 		for _, depName := range task.Depends {
// 			edges = append(edges, dag.BasicEdge(task.Name, depName))
// 		}
// 	}

// 	for _, edge := range edges {
// 		g.Connect(edge)
// 	}

// 	err := g.Walk(func(v dag.Vertex) tfdiags.Diagnostics {
// 		var diags tfdiags.Diagnostics

// 		fmt.Println("Starting Task: ", v)
// 		time.Sleep(time.Second * 4)
// 		fmt.Println("End Task: ", v)
// 		return diags
// 	})

// 	fmt.Println(err)

// }
