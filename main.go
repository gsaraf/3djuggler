package main

import (
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/leoleovich/3djuggler/juggler"
	"github.com/leoleovich/go-gcodefeeder/gcodefeeder"
	log "github.com/sirupsen/logrus"
	"io/ioutil"
	"net/http"
	"os"
	"time"
)

var (
	octoFilename             = "3djuggler.gcode"
	buttonfile               = "/tmp/buttonpress"
	gizmostatusfile          = "/tmp/gizmostatusfile"
	jobfile                  = "/tmp/job"
	waitingForButtonInterval = time.Duration(10 * time.Minute)
	pollingInterval          = time.Duration(15 * time.Second)
)

type InternEnpoint struct {
	Api_app     string
	Api_key     string
	Api_uri     string
	job         *juggler.Job
	PrinterName string
	OfficeName  string
	log         *log.Logger
}

type Config struct {
	Listen        string
	InternEnpoint *InternEnpoint
}

func main() {
	var err error
	var configFile, logFile string
	var verbose bool

	flag.StringVar(&configFile, "config", "3djuggler.json", "Main config")
	flag.StringVar(&logFile, "log", "/var/log/3djuggler.log", "Where to log")
	flag.BoolVar(&verbose, "verbose", false, "Use verbose log output")
	flag.Parse()

	if verbose {
		log.SetLevel(log.DebugLevel)
	} else {
		log.SetLevel(log.InfoLevel)
	}
	log.SetOutput(os.Stdout)

	file, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY, 0666)
	if err == nil {
		log.SetOutput(file)
	} else {
		log.Fatal("Failed to log to file")
	}
	defer file.Close()

	http.DefaultTransport.(*http.Transport).TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	daemon := &Daemon{
		timer:  time.NewTimer(0),
		config: &Config{},
		job:    &juggler.Job{Status: juggler.StatusWaitingJob},
	}

	jsonFile, err := os.Open(configFile)
	if err != nil {
		log.Fatalf("Can't open main config: %v", err)
	}
	byteValue, err := ioutil.ReadAll(jsonFile)
	if err != nil {
		panic(fmt.Sprintf("Can't open main config: %v", err))
	}
	err = json.Unmarshal(byteValue, &daemon.config)
	if err != nil {
		panic(fmt.Sprintf("Can't decode main config: %v", err))
	}
	jsonFile.Close()

	if daemon.config.Listen == "" {
		daemon.config.Listen = "[::1]:8888"
	}
	http.HandleFunc("/info", daemon.InfoHandler)
	http.HandleFunc("/start", daemon.StartHandler)
	http.HandleFunc("/reschedule", daemon.RescheduleHandler)
	http.HandleFunc("/cancel", daemon.CancelHandler)
	go func() { log.Fatal(http.ListenAndServe(daemon.config.Listen, nil)) }()
	log.Debug("Started http server on ", daemon.config.Listen)

	daemon.buttonfile = buttonfile
	daemon.gizmostatusfile = gizmostatusfile
	daemon.jobfile = jobfile

	daemon.ie = &InternEnpoint{
		Api_app: daemon.config.InternEnpoint.Api_app,
		Api_key: daemon.config.InternEnpoint.Api_key,
		Api_uri: daemon.config.InternEnpoint.Api_uri,

		PrinterName: daemon.config.InternEnpoint.PrinterName,
		OfficeName:  daemon.config.InternEnpoint.OfficeName,
		job:         &juggler.Job{},
	}

	if err := daemon.ie.reschedule(); err != nil {
		log.Error("reschedule failed: ", err)
	}
	for {
		select {
		case <-daemon.timer.C:
			daemon.timer.Reset(pollingInterval)
			if err = daemon.ie.reportStat(); err != nil {
				log.Error(err)
			}
			log.Info("My status is: ", daemon.job.Status)

			switch daemon.job.Status {
			case juggler.StatusWaitingJob:
				if err = daemon.ie.nextJob(); err != nil {
					log.Error(err)
					break
				}
				daemon.job.Id = daemon.ie.job.Id
				daemon.job.Filename = daemon.ie.job.Filename
				daemon.job.FileContent = daemon.ie.job.FileContent
				daemon.job.Progress = daemon.ie.job.Progress
				daemon.job.Owner = daemon.ie.job.Owner
				daemon.job.Fetched = time.Now()
				daemon.job.Scheduled = time.Now().Add(waitingForButtonInterval)

				// Device reads it for status
				// TODO: delete after migration
				os.Remove(daemon.buttonfile)
				os.Remove(daemon.gizmostatusfile)

				emptyFile, err := os.Create(daemon.gizmostatusfile)
				if err != nil {
					log.Error("Unable to create gizmostatusfile: ", err)
				}
				emptyFile.Close()
				err = os.Chmod(daemon.gizmostatusfile, 0666)
				if err != nil {
					log.Error("Unable to chmod gizmostatusfile: ", err)
				}

				daemon.UpdateStatus(juggler.StatusWaitingButton)
				fallthrough

			case juggler.StatusWaitingButton:
				log.Info("Job ", daemon.job.Id, " is waiting")
				err = daemon.ie.getJob(daemon.job.Id)
				if err != nil {
					log.Error("Can't get job status from intern: ", err)
				} else {
					log.Info("Job status on intern: ", daemon.ie.job.Status)
				}
				if err == nil && daemon.ie.job.Status == juggler.StatusCancelling {
					log.Info("The job is cancelling")
					daemon.UpdateStatus(juggler.StatusCancelling)
					break
				}

				// TODO: delete after migration
				gizmostatusfileStat, err := os.Stat(daemon.gizmostatusfile)
				if err != nil {
					log.Info("Job was canceled through device, canceling")
					daemon.UpdateStatus(juggler.StatusCancelling)
				} else if gizmostatusfileStat.ModTime().Add(waitingForButtonInterval).After(time.Now()) {
					if daemon.checkButtonPressed() {
						daemon.UpdateStatus(juggler.StatusSending)
					} else {
						log.Info("Waiting ", gizmostatusfileStat.ModTime().Add(waitingForButtonInterval).Unix()-time.Now().Unix(), " more seconds for somebody to press the button")
					}
				} else if daemon.job.Scheduled.After(time.Now()) {
					log.Info("Waiting ", daemon.job.Scheduled.Unix()-time.Now().Unix(), " more seconds for somebody to press the button")
				} else {
					log.Warning("Nobody pressed the button on time")
					daemon.UpdateStatus(juggler.StatusButtonTimeout)
					log.Warning("Timeout while waiting for a job. Switching back to ", daemon.job.Status)
					daemon.UpdateStatus(juggler.StatusWaitingJob)
					daemon.job.Id = 0
					os.Remove(daemon.gizmostatusfile)
				}

			case juggler.StatusSending:
				log.Info("Sending to printer")

				log.Debug("FileSize: ", len(daemon.job.FileContent))
				err := ioutil.WriteFile(daemon.jobfile, []byte(daemon.job.FileContent), 0644)
				if err != nil {
					log.Error(err)
					break
				}

				daemon.feeder, err = gcodefeeder.NewFeeder(
					"/dev/ttyACM0",
					daemon.jobfile,
				)
				if err != nil {
					log.Error("Failed to create Feeder: ", err)
					break
				}
				daemon.UpdateStatus(juggler.StatusPrinting)

				go daemon.feeder.Feed()

			case juggler.StatusPrinting:
				log.Info("Job ", daemon.job.Id, " is currently in progress")

				// Check if status file does not exist (removed through device)
				if _, err := os.Stat(daemon.gizmostatusfile); os.IsNotExist(err) {
					log.Warning("Was canceled through device. Canceling")
					daemon.feeder.Cancel()
					daemon.UpdateStatus(juggler.StatusCancelling)
					break
				}

				err = daemon.ie.getJob(daemon.job.Id)
				if err != nil {
					log.Error("Can't report status to intern: ", err)
				}
				if err == nil && daemon.ie.job.Status == juggler.StatusCancelling {
					log.Info("Cancelling the job")
					daemon.UpdateStatus(juggler.StatusCancelling)
					daemon.feeder.Cancel()
					break
				}
				daemon.job.Progress = float64(daemon.feeder.Progress())
				daemon.job.FeederStatus = daemon.feeder.Status()
				switch daemon.job.FeederStatus {
				case gcodefeeder.Finished:
					daemon.UpdateStatus(juggler.StatusFinished)
				case gcodefeeder.Error:
					daemon.UpdateStatus(juggler.StatusCancelling)
				default:
					daemon.UpdateStatus(daemon.job.Status)
				}

			case juggler.StatusCancelling:
				fallthrough
			case juggler.StatusFinished:
				log.Info("Deleting from intern")
				err = daemon.ie.deleteJob(daemon.job)
				if err != nil {
					log.Error(err)
				}
				daemon.job.Id = 0
				daemon.UpdateStatus(juggler.StatusWaitingJob)

				// Marking device as free
				os.Remove(daemon.gizmostatusfile)
			default:
				log.Error("Job ", daemon.job, " is in a weird state")
			}
		}
	}
}
