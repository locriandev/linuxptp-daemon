package pmc

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/golang/glog"
	expect "github.com/google/goexpect"
	"github.com/openshift/linuxptp-daemon/pkg/protocol"
)

var (
	ClockClassChangeRegEx = regexp.MustCompile(`gm.ClockClass[[:space:]]+(\d+)`)
	ClockClassUpdateRegEx = regexp.MustCompile(`clockClass[[:space:]]+(\d+)`)
	GetGMSettingsRegEx    = regexp.MustCompile(`clockClass[[:space:]]+(\d+)[[:space:]]+clockAccuracy[[:space:]]+(0x\d+)`)
	CmdGetParentDataSet   = "GET PARENT_DATA_SET"
	CmdGetGMSettings      = "GET GRANDMASTER_SETTINGS_NP"
	CmdSetGMSettings      = "SET GRANDMASTER_SETTINGS_NP"
	// GET GRANDMASTER_SETTINGS_NP sometimes takes more than 2 seconds
	cmdTimeout = 5000 * time.Millisecond
)

// RunPMCExp ... go expect to run PMC util cmd
func RunPMCExp(configFileName, cmdStr string, promptRE *regexp.Regexp) (result string, matches []string, err error) {
	glog.Infof("pmc read config from /var/run/%s", configFileName)
	glog.Infof("pmc run command: %s", cmdStr)
	e, _, err := expect.Spawn(fmt.Sprintf("pmc -u -b 1 -f /var/run/%s", configFileName), -1)
	if err != nil {
		return "", []string{}, err
	}
	defer e.Close()
	if err = e.Send(cmdStr + "\n"); err == nil {
		result, matches, err = e.Expect(promptRE, cmdTimeout)
		if err != nil {
			glog.Errorf("pmc result match error %s", err)
			return
		}
		glog.Infof("pmc result: %s", result)
		err = e.Send("\x03")
	}
	return
}

// RunPMCExpGetGMSettings ... get current GRANDMASTER_SETTINGS_NP
func RunPMCExpGetGMSettings(configFileName string) (g protocol.GrandmasterSettings, err error) {
	cmdStr := CmdGetGMSettings
	glog.Infof("pmc read config from /var/run/%s", configFileName)
	glog.Infof("pmc run command: %s", cmdStr)
	e, _, err := expect.Spawn(fmt.Sprintf("pmc -u -b 1 -f /var/run/%s", configFileName), -1)
	if err != nil {
		return g, err
	}
	defer e.Close()
	if err = e.Send(cmdStr + "\n"); err == nil {
		result, matches, err := e.Expect(regexp.MustCompile(g.RegEx()), cmdTimeout)
		if err != nil {
			fmt.Printf("pmc result match error %s\n", err)
			return g, err
		}
		glog.Infof("pmc result: %s", result)
		for i, m := range matches[1:] {
			g.Update(g.Keys()[i], m)
		}
		err = e.Send("\x03")
	}
	return
}

// RunPMCExpSetGMSettings ... set GRANDMASTER_SETTINGS_NP
func RunPMCExpSetGMSettings(configFileName string, g protocol.GrandmasterSettings) (err error) {
	cmdStr := CmdSetGMSettings
	cmdStr += strings.Replace(g.String(), "\n", " ", -1)
	glog.Infof("pmc read config from /var/run/%s", configFileName)
	glog.Infof("pmc run command: %s", cmdStr)
	e, _, err := expect.Spawn(fmt.Sprintf("pmc -u -b 1 -f /var/run/%s", configFileName), -1)
	if err != nil {
		return err
	}
	defer e.Close()
	if err = e.Send(cmdStr + "\n"); err == nil {
		result, _, err := e.Expect(regexp.MustCompile(g.RegEx()), cmdTimeout)
		if err != nil {
			fmt.Printf("pmc result match error %s\n", err)
			return err
		}
		glog.Infof("pmc result: %s", result)
		err = e.Send("\x03")
	}
	return
}
