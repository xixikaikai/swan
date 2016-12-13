package state

import (
	"fmt"
	"strings"
	"sync"
	"time"

	swanevent "github.com/Dataman-Cloud/swan/src/manager/event"
	"github.com/Dataman-Cloud/swan/src/mesosproto/mesos"
	"github.com/Dataman-Cloud/swan/src/types"

	"github.com/Sirupsen/logrus"
)

//  TASK_STAGING = 6;  // Initial state. Framework status updates should not use.
//  TASK_STARTING = 0; // The task is being launched by the executor.
//  TASK_RUNNING = 1;
//  TASK_KILLING = 8;  // The task is being killed by the executor.
//  TASK_FINISHED = 2; // TERMINAL: The task finished successfully.
//  TASK_FAILED = 3;   // TERMINAL: The task failed to finish successfully.
//  TASK_KILLED = 4;   // TERMINAL: The task was killed by the executor.
//  TASK_ERROR = 7;    // TERMINAL: The task description contains an error.
//  TASK_LOST = 5;     // TERMINAL: The task failed but can be rescheduled.
//  TASK_DROPPED = 9;  // TERMINAL.
//  TASK_UNREACHABLE = 10;
//  TASK_GONE = 11;    // TERMINAL.
//  TASK_GONE_BY_OPERATOR = 12;
//  TASK_UNKNOWN = 13;

const (
	SLOT_STATE_PENDING_OFFER = "slot_task_pending_offer"
	SLOT_STATE_PENDING_KILL  = "slot_task_pending_killed"

	SLOT_STATE_TASK_STAGING          = "slot_task_staging"
	SLOT_STATE_TASK_STARTING         = "slot_task_starting"
	SLOT_STATE_TASK_RUNNING          = "slot_task_running"
	SLOT_STATE_TASK_KILLING          = "slot_task_killing"
	SLOT_STATE_TASK_FINISHED         = "slot_task_finished"
	SLOT_STATE_TASK_FAILED           = "slot_task_failed"
	SLOT_STATE_TASK_KILLED           = "slot_task_killed"
	SLOT_STATE_TASK_ERROR            = "slot_task_error"
	SLOT_STATE_TASK_LOST             = "slot_task_lost"
	SLOT_STATE_TASK_DROPPED          = "slot_task_dropped"
	SLOT_STATE_TASK_UNREACHABLE      = "slot_task_unreachable"
	SLOT_STATE_TASK_GONE             = "slot_task_gone"
	SLOT_STATE_TASK_GONE_BY_OPERATOR = "slot_task_gone_by_operator"
	SLOT_STATE_TASK_UNKNOWN          = "slot_task_unknown"
)

type SlotStateCallbackFuncs func(slot *Slot)

type Slot struct {
	Index           int
	Id              string
	App             *App
	Version         *types.Version
	State           string
	StatesCallbacks map[string][]SlotStateCallbackFuncs

	CurrentTask *Task
	TaskHistory []*Task

	OfferId       string
	AgentId       string
	Ip            string
	AgentHostName string

	resourceReservationLock sync.Mutex

	MarkForDeletion      bool
	MarkForRollingUpdate bool

	restartPolicy *RestartPolicy
}

func NewSlot(app *App, version *types.Version, index int) *Slot {
	slot := &Slot{
		Index:       index,
		App:         app,
		Version:     version,
		TaskHistory: make([]*Task, 0),
		Id:          fmt.Sprintf("%d-%s-%s-%s", index, version.AppId, version.RunAs, app.MesosConnector.ClusterId), // should be app.AppId

		resourceReservationLock: sync.Mutex{},

		MarkForRollingUpdate: false,
		MarkForDeletion:      false,
		StatesCallbacks:      make(map[string][]SlotStateCallbackFuncs),
	}

	slot.DispatchNewTask(slot.Version)

	// initialize restart policy
	testAndRestartFunc := func(s *Slot) bool {
		if slot.Abnormal() {
			s.Archive()
			s.DispatchNewTask(slot.Version)
		} else {
			return true
		}

		return false
	}

	//slot.restartPolicy = NewRestartPolicy(slot, slot.Version.BackoffSeconds,
	//slot.Version.BackoffFactor, slot.Version.MaxLaunchDelaySeconds, testAndRestartFunc)
	slot.restartPolicy = NewRestartPolicy(slot, time.Second*30, 1.2, time.Second*300, testAndRestartFunc)

	return slot
}

// kill task doesn't need cleanup slot from app.Slots
func (slot *Slot) KillTask() {
	slot.StopRestartPolicy()
	slot.SetState(SLOT_STATE_PENDING_KILL)
	slot.CurrentTask.Kill()
}

// kill task and make slot sweeped after successfully kill task
func (slot *Slot) Kill() {
	slot.StopRestartPolicy()

	slot.MarkForDeletion = true
	slot.SetState(SLOT_STATE_PENDING_KILL)

	slot.CurrentTask.Kill()
}

func (slot *Slot) Archive() {
	slot.TaskHistory = append(slot.TaskHistory, slot.CurrentTask)
}

func (slot *Slot) DispatchNewTask(version *types.Version) {
	slot.Version = version
	slot.CurrentTask = NewTask(slot.App, slot.Version, slot)
	slot.SetState(SLOT_STATE_PENDING_OFFER)

	slot.App.OfferAllocatorRef.PutSlotBackToPendingQueue(slot)

}

func (slot *Slot) Update(version *types.Version) {
	logrus.Infof("update slot %s with version ID %s", slot.Id, version.ID)

	slot.MarkForRollingUpdate = true // mark as in progress of rolling update

	onSlotFinished := func(slot *Slot) {
		logrus.Infof("onSlotFinished func")
		slot.Archive()
		slot.DispatchNewTask(version)
	}
	slot.RegisterStateCallbacks(SLOT_STATE_TASK_FINISHED, onSlotFinished)

	slot.KillTask() // kill task but doesn't clean slot
}

func (slot *Slot) TestOfferMatch(ow *OfferWrapper) bool {
	if slot.Version.Constraints != nil && len(slot.Version.Constraints) > 0 {
		constraints := slot.filterConstraints(slot.Version.Constraints)
		for _, constraint := range constraints {
			cons := strings.Split(constraint, ":")
			if cons[1] == "LIKE" {
				for _, attr := range ow.Offer.Attributes {
					var value string
					name := attr.GetName()
					switch attr.GetType() {
					case mesos.Value_SCALAR:
						value = fmt.Sprintf("%d", *attr.GetScalar().Value)
					case mesos.Value_TEXT:
						value = fmt.Sprintf("%s", *attr.GetText().Value)
					default:
						logrus.Errorf("Unsupported attribute value: %s", attr.GetType())
					}

					if name == cons[0] &&
						strings.Contains(value, cons[2]) &&
						ow.CpuRemain() > slot.Version.Cpus &&
						ow.MemRemain() > slot.Version.Mem &&
						ow.DiskRemain() > slot.Version.Disk {
						return true
					}
				}
				return false
			}
		}
	}

	return ow.CpuRemain() > slot.Version.Cpus &&
		ow.MemRemain() > slot.Version.Mem &&
		ow.DiskRemain() > slot.Version.Disk
}

func (slot *Slot) filterConstraints(constraints []string) []string {
	filteredConstraints := make([]string, 0)
	for _, constraint := range constraints {
		cons := strings.Split(constraint, ":")
		if len(cons) > 3 || len(cons) < 2 {
			logrus.Errorf("Malformed Constraints")
			continue
		}

		if cons[1] != "UNIQUE" && cons[1] != "LIKE" {
			logrus.Errorf("Constraints operator %s not supported", cons[1])
			continue
		}

		if cons[1] == "UNIQUE" && strings.ToLower(cons[0]) != "hostname" {
			logrus.Errorf("Constraints operator UNIQUE only support 'hostname': %s", cons[0])
			continue
		}

		if cons[1] == "LIKE" && len(cons) < 3 {
			logrus.Errorf("Constraints operator LIKE required two operands")
			continue
		}

		filteredConstraints = append(filteredConstraints, constraint)
	}

	return filteredConstraints
}

func (slot *Slot) ReserveOfferAndPrepareTaskInfo(ow *OfferWrapper) (*OfferWrapper, *mesos.TaskInfo) {
	slot.resourceReservationLock.Lock()
	defer slot.resourceReservationLock.Unlock()

	ow.CpusUsed += slot.Version.Cpus
	ow.MemUsed += slot.Version.Mem
	ow.DiskUsed += slot.Version.Disk

	taskInfo := slot.CurrentTask.PrepareTaskInfo(ow)

	if slot.App.IsReplicates() { // reserve port only for replicates application
		ow.PortUsedSize += len(slot.Version.Container.Docker.PortMappings)
	}

	return ow, taskInfo
}

func (slot *Slot) Resources() []*mesos.Resource {
	var resources = []*mesos.Resource{}

	if slot.Version.Cpus > 0 {
		resources = append(resources, createScalarResource("cpus", slot.Version.Cpus))
	}

	if slot.Version.Mem > 0 {
		resources = append(resources, createScalarResource("mem", slot.Version.Cpus))
	}

	if slot.Version.Disk > 0 {
		resources = append(resources, createScalarResource("disk", slot.Version.Disk))
	}

	return resources
}

func (slot *Slot) StateIs(state string) bool {
	return slot.State == state
}

func (slot *Slot) SetState(state string) error {
	slot.State = state

	// handle callback
	if len(slot.StatesCallbacks[slot.State]) > 0 {
		for _, cb := range slot.StatesCallbacks[slot.State] {
			cb(slot)
		}
	}
	slot.RemoveStateCallbacks(slot.State)

	logrus.Infof("setting state for slot %s to %s", slot.Id, slot.State)

	switch slot.State {
	case SLOT_STATE_PENDING_KILL:
		slot.EmitTaskEvent(swanevent.EventTypeTaskRm)

	case SLOT_STATE_TASK_KILLED:
		slot.StopRestartPolicy()
	case SLOT_STATE_TASK_FINISHED:
		slot.StopRestartPolicy()
	case SLOT_STATE_TASK_RUNNING:
		slot.EmitTaskEvent(swanevent.EventTypeTaskAdd)
	case SLOT_STATE_TASK_LOST:

	case SLOT_STATE_TASK_FAILED:
		slot.EmitTaskEvent(swanevent.EventTypeTaskRm)
		// restart if needed
	default:
	}

	// skip app invalidation if slot state is not mesos driven
	if (slot.State != SLOT_STATE_PENDING_OFFER) ||
		(slot.State != SLOT_STATE_PENDING_KILL) {
		slot.App.InvalidateSlots()
	}
	return nil
}

func (slot *Slot) AppendStateCallbacks(state string, callback SlotStateCallbackFuncs) {
	slot.StatesCallbacks[state] = append(slot.StatesCallbacks[state], callback)
}

// empty callback queue first
func (slot *Slot) RegisterStateCallbacks(state string, callback SlotStateCallbackFuncs) {
	slot.RemoveStateCallbacks(state)
	slot.StatesCallbacks[state] = append(slot.StatesCallbacks[state], callback)
}

func (slot *Slot) RemoveStateCallbacks(state string) {
	slot.StatesCallbacks[state] = make([]SlotStateCallbackFuncs, 0)
}

func (slot *Slot) StopRestartPolicy() {
	if slot.restartPolicy != nil {
		slot.restartPolicy.Stop()
		slot.restartPolicy = nil
	}
}

func (slot *Slot) Abnormal() bool {
	return slot.StateIs(SLOT_STATE_TASK_LOST) || slot.StateIs(SLOT_STATE_TASK_FAILED) || slot.StateIs(SLOT_STATE_TASK_LOST) || slot.StateIs(SLOT_STATE_TASK_FINISHED)
}

func (slot *Slot) Normal() bool {
	return slot.StateIs(SLOT_STATE_PENDING_OFFER) || slot.StateIs(SLOT_STATE_TASK_RUNNING) || slot.StateIs(SLOT_STATE_TASK_STARTING) || slot.StateIs(SLOT_STATE_TASK_STAGING)
}

func (slot *Slot) EmitTaskEvent(t string) {
	for _, port := range slot.CurrentTask.HostPorts {
		e := &swanevent.Event{Type: t}
		e.Payload = &swanevent.TaskInfo{
			Ip:     slot.AgentHostName,
			Port:   fmt.Sprintf("%d", port),
			TaskId: "task" + strings.ToLower(strings.Replace(slot.Id, "-", ".", -1)),
			Type:   "srv",
		}

		slot.App.EmitEvent(e)
	}
}