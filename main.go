package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/leoleovich/go-gcodefeeder/gcodefeeder"
	log "github.com/sirupsen/logrus"
	"io/ioutil"
	"net/http"
	"net/url"
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

type Job struct {
	Id           int
	Filename     string `json:"file_name"`
	FileContent  string `json:"file_content"`
	Owner        string
	Status       string
	Progress     float64
	feederStatus gcodefeeder.Status
}

type InternEnpoint struct {
	Api_app     string
	Api_key     string
	Api_uri     string
	job         *Job
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
		job:    &Job{Status: "Waiting for job"},
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
	go func() { log.Fatal(http.ListenAndServe(daemon.config.Listen, nil)) }()
	log.Debug("Started http server on ", daemon.config.Listen)

	daemon.buttonfile = buttonfile
	daemon.gizmostatusfile = gizmostatusfile
	daemon.jobfile = jobfile

	ie := &InternEnpoint{
		Api_app: daemon.config.InternEnpoint.Api_app,
		Api_key: daemon.config.InternEnpoint.Api_key,
		Api_uri: daemon.config.InternEnpoint.Api_uri,

		PrinterName: daemon.config.InternEnpoint.PrinterName,
		OfficeName:  daemon.config.InternEnpoint.OfficeName,
		job:         &Job{},
	}

	if err := ie.reschedule(); err != nil {
		log.Error("reschedule failed: ", err)
	}
	for {
		select {
		case <-daemon.timer.C:
			daemon.timer.Reset(pollingInterval)
			if err = ie.reportStat(); err != nil {
				log.Error(err)
			}
			log.Info("My status is: ", daemon.job.Status)

			switch daemon.job.Status {
			case "Waiting for job":
				if err = ie.nextJob(); err != nil {
					log.Error(err)
					break
				}
				daemon.job.Id = ie.job.Id
				daemon.job.Filename = ie.job.Filename
				daemon.job.FileContent = ie.job.FileContent
				daemon.job.Status = "Waiting for a button"
				daemon.job.Progress = ie.job.Progress
				daemon.job.Owner = ie.job.Owner

				if err = ie.reportJobStatusChange(daemon.job); err != nil {
					log.Error("Can't report it to intern: ", err)
				}
				os.Remove(daemon.buttonfile)
				os.Remove(daemon.gizmostatusfile)

				// Device reads it for status
				emptyFile, err := os.Create(daemon.gizmostatusfile)
				if err != nil {
					log.Error("Unable to create gizmostatusfile: ", err)
				}
				emptyFile.Close()
				err = os.Chmod(daemon.gizmostatusfile, 0666)
				if err != nil {
					log.Error("Unable to chmod gizmostatusfile: ", err)
				}

				log.Info("The job successfully marked as ", daemon.job.Status)
				fallthrough

			case "Waiting for a button":
				log.Info("Job ", daemon.job.Id, " is waiting")
				err = ie.getJob(daemon.job.Id)
				if err != nil {
					log.Error("Can't get job status from intern: ", err)
				} else {
					log.Info("Job status on intern: ", ie.job.Status)
				}
				if err == nil && ie.job.Status == "Cancelling" {
					log.Info("The job is cancelling")
					daemon.job.Status = "Cancelling"
					break
				}
				gizmostatusfileStat, err := os.Stat(daemon.gizmostatusfile)
				if err != nil {
					log.Info("Job was canceled through device, canceling")
					daemon.job.Status = "Cancelling"
					if err = ie.reportJobStatusChange(daemon.job); err != nil {
						log.Error("Can't report it to intern: ", err)
					}
				} else if gizmostatusfileStat.ModTime().Add(waitingForButtonInterval).After(time.Now()) {
					if daemon.checkButtonPressed() {
						daemon.job.Status = "Sending to printer"
						if err = ie.reportJobStatusChange(daemon.job); err != nil {
							log.Error("Can't report it to intern: ", err)
						}
					} else {
						log.Info("Waiting ", gizmostatusfileStat.ModTime().Add(waitingForButtonInterval).Unix()-time.Now().Unix(), " more seconds for somebody to press the button")
					}
				} else {
					log.Warning("Nobody pressed the button on time")
					daemon.job.Status = "Button timeout"
					if err = ie.reportJobStatusChange(daemon.job); err != nil {
						log.Error("Can't report it to intern: ", err)
					}
					log.Warning("Timeout while waiting for a job. Switching back to ", daemon.job.Status)
					daemon.job.Status = "Waiting for job"
					daemon.job.Id = 0
					os.Remove(daemon.gizmostatusfile)
				}

			case "Sending to printer":
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

				log.Info("Mark as Printing on intern")
				daemon.job.Status = "Printing"
				if err = ie.reportJobStatusChange(daemon.job); err != nil {
					log.Error("Can't report it to intern: ", err)
				}

				go daemon.feeder.Feed()

			case "Printing":
				log.Info("Job ", daemon.job.Id, " is currently in progress")

				// Check if status file does not exist (removed through device)
				if _, err := os.Stat(daemon.gizmostatusfile); os.IsNotExist(err) {
					daemon.job.Status = "Cancelling"
					log.Warning("Was canceled through device. Canceling")
					daemon.feeder.Cancel()
					if err = ie.reportJobStatusChange(daemon.job); err != nil {
						log.Error("Can't report progress to intern: ", err)
					}
					break
				}

				err = ie.getJob(daemon.job.Id)
				if err != nil {
					log.Error("Can't report status to intern: ", err)
				}
				if err == nil && ie.job.Status == "Cancelling" {
					daemon.job.Status = "Cancelling"
					if err = ie.reportJobStatusChange(daemon.job); err != nil {
						log.Error("Can't report progress to intern: ", err)
					}
					log.Info("Cancelling the job")
					daemon.feeder.Cancel()
					break
				}
				daemon.job.Progress = float64(daemon.feeder.Progress())
				daemon.job.feederStatus = daemon.feeder.Status()
				switch daemon.job.feederStatus {
				case gcodefeeder.Finished:
					daemon.job.Status = "Finished"
				case gcodefeeder.Error:
					daemon.job.Status = "Cancelling"
				}
				if err = ie.reportJobStatusChange(daemon.job); err != nil {
					log.Error("Can't report progress to intern: ", err)
				}

			case "Cancelling":
				fallthrough
			case "Finished":
				log.Info("Deleting from intern")
				err = ie.deleteJob(daemon.job)
				if err != nil {
					log.Error(err)
				}
				daemon.job.Id = 0
				daemon.job.Status = "Waiting for job"
				// Marking device as free
				os.Remove(daemon.gizmostatusfile)
			default:
				log.Error("Job ", daemon.job, " is in a weird state")
			}
		}
	}
}

func (ie *InternEnpoint) reportJobStatusChange(job *Job) error {
	statusWithProgress := job.Status
	if job.Status == "Printing" {
		switch job.feederStatus {
		case gcodefeeder.Printing:
			sofar := job.Progress
			statusWithProgress = fmt.Sprintf("Printing... (%0.1f%%)", sofar)
		case gcodefeeder.MMUBusy:
			statusWithProgress = fmt.Sprintf("Printing paused: MMU paused printing")
		case gcodefeeder.FSensorBusy:
			statusWithProgress = fmt.Sprintf("Printing paused: Filament sensor paused printing")
		}
	}

	data := url.Values{}
	data.Set("app", ie.Api_app)
	data.Add("token", ie.Api_key)
	data.Add("action", "update")
	data.Add("status", statusWithProgress)
	data.Add("id", fmt.Sprintf("%d", job.Id))
	data.Add("printer_name", ie.PrinterName)
	data.Add("office_name", ie.OfficeName)

	req, err := http.NewRequest("POST", ie.Api_uri+"/job/", bytes.NewBufferString(data.Encode()))
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

func (ie *InternEnpoint) reschedule() error {
	data := url.Values{}
	data.Set("app", ie.Api_app)
	data.Add("token", ie.Api_key)
	data.Add("action", "reschedule")
	data.Add("printer_name", ie.PrinterName)
	data.Add("office_name", ie.OfficeName)

	req, err := http.NewRequest("POST", ie.Api_uri+"/printer/", bytes.NewBufferString(data.Encode()))
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

func (ie *InternEnpoint) deleteJob(job *Job) error {
	data := url.Values{}
	data.Set("app", ie.Api_app)
	data.Add("token", ie.Api_key)
	data.Add("action", "delete")
	data.Add("id", fmt.Sprintf("%d", job.Id))
	data.Add("printer_name", ie.PrinterName)
	data.Add("office_name", ie.OfficeName)

	req, err := http.NewRequest("POST", ie.Api_uri+"/job/", bytes.NewBufferString(data.Encode()))
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

func (ie *InternEnpoint) nextJob() error {
	return ie.getJob(0)
}

func (ie *InternEnpoint) getJob(id int) error {
	data := url.Values{}
	data.Set("app", ie.Api_app)
	data.Add("token", ie.Api_key)
	data.Add("action", "get")
	data.Add("printer_name", ie.PrinterName)
	data.Add("office_name", ie.OfficeName)
	if id != 0 {
		data.Add("id", fmt.Sprint(id))
	}

	req, err := http.NewRequest("POST", ie.Api_uri+"/job/", bytes.NewBufferString(data.Encode()))
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}

	f, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var result struct {
		Success bool
		Content *Job
		Error   string
	}

	err = json.Unmarshal(f, &result)
	if err != nil {
		return err
	}
	if !result.Success {
		return fmt.Errorf("job %v action 'get' unsuccessful: %v", id, result.Error)
	}
	ie.job = result.Content

	if ie.job.Id == 0 {
		return errors.New("Nothing to print")
	}

	return nil
}

func (ie *InternEnpoint) reportStat() error {
	data := url.Values{}
	data.Set("app", ie.Api_app)
	data.Add("token", ie.Api_key)
	data.Add("action", "heartbeat")
	data.Add("printer_name", ie.PrinterName)
	data.Add("office_name", ie.OfficeName)

	req, err := http.NewRequest("POST", ie.Api_uri+"/printer/", bytes.NewBufferString(data.Encode()))
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}
