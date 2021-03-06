package main

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"time"
)

/*
TODO:
- check validation of input yaml
- check lifecycle of status of vertex
- global status for clean up
*/

//raw
type flow struct {
	Name     string `yaml:"name"`
	Schedule string `yaml:"schedule"`
	Tasks    []task `yaml:"tasks"`
	Image    string `yaml:"image"`
}

type task struct {
	Name   string              `yaml:"name"`
	Params map[string][]string `yaml:"params"`
	Next   string              `yaml:"next"`
}

//DAG processed
type DAG struct {
	Name     string
	Time     time.Time
	Vertices []vertex
	Params   []params

	msgCh chan event

	image string
}

type vertex struct {
	Name       string
	Downstream []string
	Status     string

	retry    int
	next     string
	upstream []string
	params   map[string]string

	//container info
	image       string
	containerID string
	port        string
}

type params struct {
	name  string
	index int
	value map[string]string
}

type event struct {
	// eventName   string
	name        string
	status      string
	containerID string
}

// DAGRun off
type DAGRun struct {
	DAGs []DAG
}

func newDAG(f flow) *DAG {
	d := &DAG{Name: f.Name}
	d.setVertex(f)
	d.setEdges()
	d.initLog()
	return d
}

func (d *DAG) start() {
	log.Println("started")
	d.msgCh = make(chan event)
	//vertex listen
	go d.emit()
	//DAG listen
	go d.listen()
	//start from root
	d.msgCh <- event{}
	//notify vertex everything is done
	d.handleDone()
}

func (d *DAG) setVertex(f flow) {
	for _, t := range f.Tasks {
		if len(t.Params) == 0 {
			v := vertex{Name: t.Name, next: t.Next}
			d.Vertices = append(d.Vertices, v)
		} else {
			d.setParams(t)
		}
	}

	d.setRetry()
	d.setImage(f)
}

func (d *DAG) setImage(f flow) {
	d.image = f.Image
	for i := range d.Vertices {
		d.Vertices[i].image = d.image
	}
}

func (d *DAG) setParams(t task) {
	for _, v := range t.Params {
		for i := 0; i < len(v); i++ {
			temp := map[string]string{}
			for ik, iv := range t.Params {
				temp[ik] = iv[i]
			}
			v := vertex{Name: t.Name + "-" + strconv.Itoa(i), params: temp, next: t.Next}
			d.Vertices = append(d.Vertices, v)
		}
		break
	}
}

func (d *DAG) setEdges() {
	for i := range d.Vertices {
		v := d.Vertices[i]
		if v.next != "" {
			v.Downstream = append(v.Downstream, v.next)
			for j := range d.Vertices {
				if v.next == d.Vertices[j].Name {
					d.Vertices[j].upstream = append(d.Vertices[j].upstream, v.Name)
				}
			}
		}
	}
}

func (d *DAG) emit() {
	for msg := range d.msgCh {
		for i := range d.Vertices {
			go d.Vertices[i].listen(msg, d.msgCh)
		}
	}
}

func (d *DAG) handleDone() {
	for {
		time.Sleep(1 * time.Second)
		if d.isAllDone() == true {
			log.Println("done")
			close(d.msgCh)
			break
		}
	}
}

func (d *DAG) listen() {
	go d.getStopSignal()
	//msgChan forward msg while update status
	for msg := range d.msgCh {
		d.updateStatus()
		d.msgCh <- msg
	}
}

func (d *DAG) getStopSignal() {
	for {
		time.Sleep(1 * time.Second)
		if d.Name == os.Getenv("STOP") {
			//TODO: need stopChan ? or sender check if golang channel is closed?
			d.stopAllcontainers()
			os.Setenv("STOP", "")
			close(d.msgCh)
			break
		}
	}
}

func (d *DAG) stopAllcontainers() {
	for i := range d.Vertices {
		d.Vertices[i].stopContainer()
	}
}

func (d *DAG) initLog() {
	path := d.Name + "-log.json"
	if _, err := os.Stat(path); os.IsNotExist(err) {
		d.Time = time.Now()
		var emptyArray []DAG
		run := DAGRun{DAGs: append(emptyArray, *d)}
		write(path, run)
	}
}

//TODO: update upon request?
func (d *DAG) updateStatus() {
	path := d.Name + "-log.json"
	var run DAGRun
	read(path, &run)
	run.DAGs = append(run.DAGs, *d)
	write(path, run)
}

func (d *DAG) isAllDone() bool {
	//all retry == 0 => return true
	for _, v := range d.Vertices {
		if v.retry != 0 {
			break
		}
		return true
	}

	for _, v := range d.Vertices {
		if v.Status != "succeeded" {
			return false
		}
	}

	return true
}

func (d *DAG) setRetry() {
	for i := range d.Vertices {
		d.Vertices[i].retry = 3
	}
}

func (v *vertex) handleEvent(e event) {
	fmt.Printf("%s recv from %s \n", v.Name, e)

	if v.Name == e.name {
		v.Status = e.status
		switch en := e.status; en {
		case "succeeded":
			v.stopContainer()
		case "failed":
			// TODO: need msgChan to retry or notify DAG?
			//v.run(msgChan)
		}
	} else {
		switch en := e.status; en {
		case "succeeded":
			v.removeUpstream(e.name)
		}
	}
}

func (v *vertex) startContainer() {
	v.containerID, v.port = dk.start(v.image)
}

func (v *vertex) stopContainer() {
	fmt.Println("stop: ", v.Name, v.containerID)
	dk.stop(v.containerID)
}

//listen on
func (v *vertex) listen(msg event, msgChan chan event) {
	v.handleEvent(msg)
	v.run(msgChan)
}

func (v *vertex) run(msgChan chan event) {
	//check if we can run this vertex
	if len(v.upstream) == 0 && v.Status != "succeeded" && v.Status != "running" && v.retry > 0 {
		v.Status = "running"
		v.retry--
		v.startContainer()
		go runNotebook(v.Name, v.params, v.port, msgChan)
	}
}

func (v *vertex) removeUpstream(up string) {
	if len(v.upstream) == 1 {
		var emptyupstream []string
		v.upstream = emptyupstream
		return
	}

	for i := 0; i < len(v.upstream); i++ {
		if v.upstream[i] == up {
			v.upstream = append(v.upstream[:i], v.upstream[i+1:]...)
			i--
		}
	}
}

func runNotebook(nb string, p map[string]string, port string, statusChan chan event) {
	fmt.Println("runNotebook: ", nb)
	//make request
	var r request
	r.setParams(p)
	r.port = port

	r.run(nb + ".json")

	//init event
	rst := event{name: nb}

	//Poll status
	for {
		time.Sleep(1 * time.Second)
		fmt.Println("Get status: ", nb, rst.status)
		if r.status() == "" || r.status() == "IDLE" {
			rst.status = "succeeded"
			statusChan <- rst
			break
			//TODO: other status
		} else if r.status() == "RUNNING" {
			continue
		} else {
			rst.status = "failed"
			statusChan <- rst
		}
	}
}
