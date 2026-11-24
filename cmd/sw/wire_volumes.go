package main

import (
	"log"

	"shearwater/internal/admission"
	"shearwater/internal/apis"
	"shearwater/internal/volume"
)

func wireVolumes(w *wiring) (scheduleFilter func(*apis.Pod, *apis.Node) string, admit []admission.Func) {
	if err := volume.Bootstrap(w.Store); err != nil {
		log.Fatalf("sw server: bootstrap local-path storage class: %v", err)
	}
	w.Go("persistent-volumes", volume.Controller{Store: w.Store}.Run)
	return volume.SchedulerFilter(w.Store), []admission.Func{volume.Admit}
}
