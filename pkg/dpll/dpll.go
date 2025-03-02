package dpll

import (
	"context"
	"encoding/binary"
	"fmt"
	"log"
	"math/bits"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/golang/glog"
	"github.com/mdlayher/genetlink"
	"github.com/openshift/linuxptp-daemon/pkg/config"
	nl "github.com/openshift/linuxptp-daemon/pkg/dpll-netlink"
	"github.com/openshift/linuxptp-daemon/pkg/event"
	"golang.org/x/sync/semaphore"
)

const (
	DPLL_UNKNOWN       = -1
	DPLL_INVALID       = 0
	DPLL_FREERUN       = 1
	DPLL_LOCKED        = 2
	DPLL_LOCKED_HO_ACQ = 3
	DPLL_HOLDOVER      = 4

	LocalMaxHoldoverOffSet = 1500  //ns
	LocalHoldoverTimeout   = 14400 //secs
	MaxInSpecOffset        = 100   //ns
	monitoringInterval     = 1 * time.Second

	LocalMaxHoldoverOffSetStr = "LocalMaxHoldoverOffSet"
	LocalHoldoverTimeoutStr   = "LocalHoldoverTimeout"
	MaxInSpecOffsetStr        = "MaxInSpecOffset"
)

type DpllConfig struct {
	LocalMaxHoldoverOffSet int64
	LocalHoldoverTimeout   int64
	MaxInSpecOffset        int64
	iface                  string
	name                   string
	slope                  float64
	timer                  int64 //secs
	inSpec                 bool
	offset                 int64
	state                  event.PTPState
	onHoldover             bool
	sourceLost             bool
	processConfig          config.ProcessConfig
	dependingState         []event.EventSource
	exitCh                 chan struct{}
	ticker                 *time.Ticker
	// DPLL netlink connection pointer. If 'nil', use sysfs
	conn *nl.Conn
	// We need to keep latest DPLL status values, since Netlink device
	// change indications don't contain all the status fields, but
	// only the changed one(s)
	phase_status     int64
	frequency_status int64
	phase_offset     int64
	// clockId is needed to distinguish between DPLL associated with the particular
	// iface from other DPLL units that might be present on the system. Clock ID implementation
	// is driver-specific and vendor-specific, always derived from hardware ID or MAC address
	// This clock ID value is initialized from MAC address, and updated after the first "device dump"
	//  on the basis of best correlation to the initial value
	clockId        uint64
	clockIdUpdated bool
}

func (d *DpllConfig) Name() string {
	//TODO implement me
	return "dpll"
}

func (d *DpllConfig) Stopped() bool {
	//TODO implement me
	panic("implement me")
}

// ExitCh ... exit channel
func (d *DpllConfig) ExitCh() chan struct{} {
	return d.exitCh
}

func (d *DpllConfig) CmdStop() {
	glog.Infof("stopping %s", d.Name())
	d.ticker.Stop()
	glog.Infof("Ticker stopped %s", d.Name())
	close(d.exitCh) // terminate loop
	glog.Infof("Process %s terminated", d.Name())
}

func (d *DpllConfig) CmdInit() {
	//TODO implement me
	glog.Infof("cmdInit not implemented %s", d.Name())
}

func (d *DpllConfig) CmdRun(stdToSocket bool) {
	//not implemented
}

func NewDpll(localMaxHoldoverOffSet, localHoldoverTimeout, maxInSpecOffset int64,
	iface string, dependingState []event.EventSource) *DpllConfig {
	glog.Infof("Calling NewDpll with localMaxHoldoverOffSet=%d, localHoldoverTimeout=%d, maxInSpecOffset=%d, iface=%s", localMaxHoldoverOffSet, localHoldoverTimeout, maxInSpecOffset, iface)
	d := &DpllConfig{
		LocalMaxHoldoverOffSet: localMaxHoldoverOffSet,
		LocalHoldoverTimeout:   localHoldoverTimeout,
		MaxInSpecOffset:        maxInSpecOffset,
		slope: func() float64 {
			return float64((localMaxHoldoverOffSet / localHoldoverTimeout) * 1000)
		}(),
		timer:          0,
		offset:         0,
		state:          event.PTP_FREERUN,
		iface:          iface,
		onHoldover:     false,
		sourceLost:     false,
		dependingState: dependingState,
		exitCh:         make(chan struct{}),
		ticker:         time.NewTicker(monitoringInterval),
	}
	d.timer = int64(float64(d.MaxInSpecOffset) / d.slope)
	d.initDpllClockId()
	return d
}

// nlUpdateState updates DPLL state in the DpllConfig structure.
func (d *DpllConfig) nlUpdateState(replies []*nl.DoDeviceGetReply) {
	const (
		EEC_DPLL uint32 = 0
		PPS_DPLL uint32 = 1
	)
	for _, reply := range replies {
		if !d.clockIdUpdated && reply.ClockId != d.clockId {
			if bits.OnesCount64(reply.ClockId^d.clockId) <= 4 {
				d.clockId = reply.ClockId
				d.clockIdUpdated = true
				glog.Infof("clock ID associated with %s is set to %x", d.iface, d.clockId)
			} else {
				continue
			}
		}
		if reply.ClockId == d.clockId {
			glog.Info(nl.GetDpllStatusHR(reply))
			switch reply.Id {
			case EEC_DPLL:
				d.frequency_status = int64(reply.LockStatus)
			case PPS_DPLL:
				d.phase_status = int64(reply.LockStatus)
				d.phase_offset = 0 // TODO: get offset from reply when implemented
			}
		}
	}
}

// monitorNtf receives a multicast unsolicited notification and
// calls dpll state updating function.
func (d *DpllConfig) monitorNtf(c *genetlink.Conn, holdoverCloseCh chan bool) {
	for {
		msgs, _, err := c.Receive()
		if err != nil {
			glog.Error(err)
			return
		}
		replies, err := nl.ParseDeviceReplies(msgs)
		if err != nil {
			glog.Error(err)
			return
		}
		d.nlUpdateState(replies)
		d.updateDependingProcessState()
		d.stateDecision(holdoverCloseCh)

	}
}

// checks whether or not sysfs file structure exists for dpll associated with the interface
func (d *DpllConfig) isSysFsPresent() bool {
	path := fmt.Sprintf("/sys/class/net/%s/device/dpll_0_state", d.iface)
	if _, err := os.Stat(path); err == nil {
		return true
	}
	return false
}

// MonitorDpllNetlink monitors DPLL through netlink
func (d *DpllConfig) MonitorDpllNetlink() {
	var holdoverCloseCh chan bool
	redial := true
	var replies []*nl.DoDeviceGetReply
	var err error
	var sem *semaphore.Weighted
	for {

		// If netlink connection failed before
		if redial {
			if d.conn == nil {
				conn, err := nl.Dial(nil)
				if err != nil {
					d.conn = nil
					glog.Info("can't dial to DPLL netlink: ", err)
					goto checkExit

				} else {
					d.conn = conn
				}
			}
			c := d.conn.GetGenetlinkConn()
			mcastId, found := d.conn.GetMcastGroupId(nl.DPLL_MCGRP_MONITOR)
			if !found {
				glog.Warning("multicast ID ", nl.DPLL_MCGRP_MONITOR, " not found")
				goto abort
			}
			replies, err = d.conn.DumpDeviceGet()
			if err != nil {
				goto abort
			}
			d.nlUpdateState(replies)
			d.updateDependingProcessState()
			d.stateDecision(holdoverCloseCh)
			err = c.JoinGroup(mcastId)
			if err != nil {
				goto abort
			}
			sem = semaphore.NewWeighted(1)
			err = sem.Acquire(context.Background(), 1)
			if err != nil {
				goto abort
			}

			go func() {
				defer sem.Release(1)
				d.monitorNtf(c, holdoverCloseCh)
			}()
			goto checkExit
		abort:
			d.stopDpll()
		}

	checkExit:
		select {
		case <-d.exitCh:
			glog.Infof("terminating netlink DPLL monitoring")
			d.processConfig.EventChannel <- event.EventChannel{
				ProcessName: event.DPLL,
				IFace:       d.iface,
				CfgName:     d.processConfig.ConfigName,
				ClockType:   d.processConfig.ClockType,
				Time:        time.Now().Unix(),
				Reset:       true,
			}
			if d.onHoldover {
				close(holdoverCloseCh) // cancel any holdover
			}
			d.conn.Close()
			glog.Infof("Netlink connection %s is closed", d.Name())
			return
		default:
			ctx, cancel := context.WithTimeout(context.Background(), time.Second*2)
			defer cancel()
			err = sem.Acquire(ctx, 1)
			if err != nil {
				glog.Info("dpll monitoring is running")
				redial = false
			} else {
				glog.Info("dpll monitoring exited, redial")
				d.stopDpll()
				redial = true
			}
		}
	}
}

// stopDpll stops DPLL monitoring
func (d *DpllConfig) stopDpll() {
	d.conn.Close()
	d.conn = nil
}

// MonitorProcess is initiating monitoring of DPLL associated with a process
func (d *DpllConfig) MonitorProcess(processCfg config.ProcessConfig) {
	d.processConfig = processCfg
	go d.MonitorDpll()
}

// MonitorDpll tries to create netlink connection to DPLL.
// If netlink can't be dialed for N retries and sysfs directory
// structure is present, fall back to sysfs.
// Otherwise, use netlink and never fall back
func (d *DpllConfig) MonitorDpll() {
	if d.isSysFsPresent() {
		d.MonitorDpllSysfs()

	} else {
		d.MonitorDpllNetlink()
	}
}

// initDpllClockId finds clock ID associated with the network interface
// and initializes it in the DpllConfig structure.
// We need Clock ID to select DPLL belonging to the right card, if several cards
// are available on the system.
// TODO: read clock ID from PCI as extended capabilities DSN (intel implementation).
// TODO: even then, this will work for Intel, but might be different for other vendors.
func (d *DpllConfig) initDpllClockId() error {
	const (
		EUI48 = 6
		EUI64 = 8
	)
	interfaces, err := net.Interfaces()
	if err != nil {
		log.Fatal(err)
	}
	var mac net.HardwareAddr
	for _, ifc := range interfaces {
		if ifc.Name == d.iface {
			mac = ifc.HardwareAddr
		}
	}
	var clockId []byte
	if len(mac) == EUI48 {
		clockId = []byte{mac[0], mac[1], mac[2], 0xff, 0xff, mac[3], mac[4], mac[5]}
	} else if len(mac) == EUI64 {
		clockId = mac
	} else {
		return fmt.Errorf("can't find mac address of interface %s", d.iface)
	}
	d.clockId = binary.BigEndian.Uint64(clockId)
	return nil
}

// updateDependingProcessState updates d.sourceLost according to the process response
// Currently, GNSS is the only source we take into account. The function is
// called periodically.
func (d *DpllConfig) updateDependingProcessState() {
	responseChannel := make(chan event.PTPState)
	var responseState event.PTPState
	lowestState := event.PTP_UNKNOWN
	var dependingProcessState []event.PTPState
	for _, stateSource := range d.dependingState {
		event.GetPTPStateRequest(event.StatusRequest{
			Source:          stateSource,
			CfgName:         d.processConfig.ConfigName,
			ResponseChannel: responseChannel,
		})
		select {
		case responseState = <-responseChannel:
		case <-time.After(200 * time.Millisecond): //TODO:move this to non blocking call
			responseState = event.PTP_UNKNOWN
		}
		dependingProcessState = append(dependingProcessState, responseState)
	}
	for i, state := range dependingProcessState {
		if i == 0 || state < lowestState {
			lowestState = state
		}
	}
	// check dpll status
	if lowestState == event.PTP_LOCKED {
		d.sourceLost = false
	} else {
		d.sourceLost = true
	}
}

// stateDecision
func (d *DpllConfig) stateDecision(holdoverCloseCh chan bool) {

	// send event
	// calculate dpll status
	dpllStatus := d.getWorseState(d.phase_status, d.frequency_status)
	glog.Infof("dpll decision entry: status %d, offset %d, in spec %v, sourceLost %v, on holdover %v",
		dpllStatus, d.offset, d.inSpec, d.sourceLost, d.onHoldover)
	switch dpllStatus {
	case DPLL_FREERUN, DPLL_INVALID, DPLL_UNKNOWN:
		d.inSpec = true
		if d.onHoldover {
			holdoverCloseCh <- true
		}
		d.state = event.PTP_FREERUN
	case DPLL_LOCKED:
		d.inSpec = true
		if !d.sourceLost && d.isOffsetInRange() {
			d.state = event.PTP_LOCKED
		} else {
			d.state = event.PTP_FREERUN
		}
	case DPLL_LOCKED_HO_ACQ, DPLL_HOLDOVER:
		if !d.sourceLost && d.isOffsetInRange() {
			d.inSpec = true
			d.state = event.PTP_LOCKED
			if d.onHoldover {
				d.inSpec = true
				holdoverCloseCh <- true
			}
		} else if d.sourceLost && d.inSpec {
			holdoverCloseCh = make(chan bool)
			d.inSpec = false
			go d.holdover(holdoverCloseCh)
		} else {
			d.state = event.PTP_FREERUN
		}
	}
	d.processConfig.EventChannel <- event.EventChannel{
		ProcessName: event.DPLL,
		State:       d.state,
		IFace:       d.iface,
		CfgName:     d.processConfig.ConfigName,
		Values: map[event.ValueType]int64{
			event.FREQUENCY_STATUS: d.frequency_status,
			event.OFFSET:           d.phase_offset,
			event.PHASE_STATUS:     d.phase_status,
		},
		ClockType:  d.processConfig.ClockType,
		Time:       time.Now().Unix(),
		WriteToLog: true,
		Reset:      false,
	}

}

// MonitorDpllSysfs ... monitor dpll events
func (d *DpllConfig) MonitorDpllSysfs() {
	defer func() {
		if recover() != nil {
			// handle closed close channel
		}
	}()

	d.ticker = time.NewTicker(monitoringInterval)

	var holdoverCloseCh chan bool

	// determine dpll state
	d.inSpec = true
	for {
		select {
		case <-d.exitCh:
			glog.Infof("terminating sysfs DPLL monitoring")
			d.processConfig.EventChannel <- event.EventChannel{
				ProcessName: event.DPLL,
				IFace:       d.iface,
				CfgName:     d.processConfig.ConfigName,
				ClockType:   d.processConfig.ClockType,
				Time:        time.Now().Unix(),
				Reset:       true,
			}
			if d.onHoldover {
				close(holdoverCloseCh) // cancel any holdover
			}
			return
		case <-d.ticker.C:
			// monitor DPLL
			d.updateDependingProcessState()
			d.phase_status, d.frequency_status, d.phase_offset = d.sysfs(d.iface)
			d.stateDecision(holdoverCloseCh)
		}
	}
}

// getStateQuality maps the state with relatively worse signal quality with
// a lower number for easy comparison
// Ref: ITU-T G.781 section 6.3.1 Auto selection operation
func (d *DpllConfig) getStateQuality() map[int64]float64 {
	return map[int64]float64{
		DPLL_UNKNOWN:       -1,
		DPLL_INVALID:       0,
		DPLL_FREERUN:       1,
		DPLL_HOLDOVER:      2,
		DPLL_LOCKED:        3,
		DPLL_LOCKED_HO_ACQ: 4,
	}
}

// getWorseState returns the state with worse signal quality
func (d *DpllConfig) getWorseState(pstate, fstate int64) int64 {
	sq := d.getStateQuality()
	if sq[pstate] < sq[fstate] {
		return pstate
	}
	return fstate
}

func (d *DpllConfig) holdover(closeCh chan bool) {
	start := time.Now()
	ticker := time.NewTicker(1 * time.Second)
	d.state = event.PTP_HOLDOVER
	for timeout := time.After(time.Duration(d.timer)); ; {
		select {
		case <-ticker.C:
			//calculate offset
			d.offset = int64(d.slope * float64(time.Since(start)))
		case <-timeout:
			d.state = event.PTP_FREERUN
			return
		case <-closeCh:
			return
		}
	}
}

func (d *DpllConfig) isOffsetInRange() bool {
	if d.offset <= d.processConfig.GMThreshold.Max && d.offset >= d.processConfig.GMThreshold.Min {
		return true
	}
	glog.Infof("dpll offset out of range: min %d, max %d, current %d",
		d.processConfig.GMThreshold.Min, d.processConfig.GMThreshold.Max, d.offset)
	return false
}

// Index of DPLL being configured [0:EEC (DPLL0), 1:PPS (DPLL1)]
// Frequency State (EEC_DPLL)
// cat /sys/class/net/interface_name/device/dpll_0_state
// Phase State
// cat /sys/class/net/ens7f0/device/dpll_1_state
// Phase Offset
// cat /sys/class/net/ens7f0/device/dpll_1_offset
func (d *DpllConfig) sysfs(iface string) (phaseState, frequencyState, phaseOffset int64) {
	var err error
	var content []byte
	var contentStr string
	if iface == "" {
		phaseState = DPLL_INVALID
		frequencyState = DPLL_INVALID
		phaseOffset = 0
		return
	}
	frequencyStateStr := fmt.Sprintf("/sys/class/net/%s/device/dpll_0_state", iface)
	phaseStateStr := fmt.Sprintf("/sys/class/net/%s/device/dpll_1_state", iface)
	phaseOffsetStr := fmt.Sprintf("/sys/class/net/%s/device/dpll_1_offset", iface)
	// Read the content of the sysfs path
	content, err = os.ReadFile(frequencyStateStr)
	if err != nil {
		glog.Errorf("error reading sysfs path %s %s:", frequencyStateStr, err)
	} else {
		contentStr = strings.ReplaceAll(string(content), "\n", "")
		if frequencyState, err = strconv.ParseInt(contentStr, 10, 64); err != nil {
			glog.Errorf("error parsing frequency state %s = %s:", contentStr, err)
		}
	}
	// Read the content of the sysfs path
	if content, err = os.ReadFile(phaseStateStr); err != nil {
		glog.Errorf("error reading sysfs path %s %s:", phaseStateStr, err)
	} else {
		contentStr = strings.ReplaceAll(string(content), "\n", "")
		if phaseState, err = strconv.ParseInt(contentStr, 10, 64); err != nil {
			glog.Errorf("error parsing phase state %s = %s:", contentStr, err)
		}
	}
	// Read the content of the sysfs path
	if content, err = os.ReadFile(phaseOffsetStr); err != nil {
		glog.Errorf("error reading sysfs path %s %s:", phaseOffsetStr, err)
	} else {
		contentStr = strings.ReplaceAll(string(content), "\n", "")
		if phaseOffset, err = strconv.ParseInt(contentStr, 10, 64); err != nil {
			glog.Errorf("error parsing phase offset %s=%s:", contentStr, err)
		}
		phaseOffset = phaseOffset / 100 // convert to nanoseconds from tens of picoseconds (div by 100)
	}
	return
}
