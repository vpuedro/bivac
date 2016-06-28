package handler

import (
	"bytes"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"sort"
	"strings"

	"golang.org/x/net/context"

	log "github.com/Sirupsen/logrus"
	"github.com/camptocamp/conplicity/util"
	docker "github.com/docker/engine-api/client"
	"github.com/docker/engine-api/types"
	"github.com/docker/engine-api/types/container"
	"github.com/jessevdk/go-flags"
)

type config struct {
	Version          bool     `short:"V" long:"version" description:"Display version."`
	Image            string   `short:"i" long:"image" description:"The duplicity docker image." env:"DUPLICITY_DOCKER_IMAGE" default:"camptocamp/duplicity:latest"`
	Loglevel         string   `short:"l" long:"loglevel" description:"Set loglevel ('debug', 'info', 'warn', 'error', 'fatal', 'panic')." env:"CONPLICITY_LOG_LEVEL" default:"info"`
	VolumesBlacklist []string `short:"b" long:"blacklist" description:"Volumes to blacklist in backups." env:"CONPLICITY_VOLUMES_BLACKLIST" env-delim:","`
	Manpage          bool     `short:"m" long:"manpage" description:"Output manpage."`
	NoVerify         bool     `long:"no-verify" description:"Do not verify backup." env:"CONPLICITY_NO_VERIFY"`
	JSON             bool     `short:"j" long:"json" description:"Log as JSON (to stderr)." env:"JSON_OUTPUT"`

	Duplicity struct {
		TargetURL       string `short:"u" long:"url" description:"The duplicity target URL to push to." env:"DUPLICITY_TARGET_URL"`
		FullIfOlderThan string `long:"full-if-older-than" description:"The number of days after which a full backup must be performed." env:"CONPLICITY_FULL_IF_OLDER_THAN" default:"15D"`
		RemoveOlderThan string `long:"remove-older-than" description:"The number days after which backups must be removed." env:"CONPLICITY_REMOVE_OLDER_THAN" default:"30D"`
	} `group:"Duplicity Options"`

	Metrics struct {
		PushgatewayURL string `short:"g" long:"gateway-url" description:"The prometheus push gateway URL to use." env:"PUSHGATEWAY_URL"`
	} `group:"Metrics Options"`

	AWS struct {
		AccessKeyID     string `long:"aws-access-key-id" description:"The AWS access key ID." env:"AWS_ACCESS_KEY_ID"`
		SecretAccessKey string `long:"aws-secret-key-id" description:"The AWS secret access key." env:"AWS_SECRET_ACCESS_KEY"`
	} `group:"AWS Options"`

	Swift struct {
		Username   string `long:"swift-username" description:"The Swift user name." env:"SWIFT_USERNAME"`
		Password   string `long:"swift-password" description:"The Swift password." env:"SWIFT_PASSWORD"`
		AuthURL    string `long:"swift-auth_url" description:"The Swift auth URL." env:"SWIFT_AUTHURL"`
		TenantName string `long:"swift-tenant-name" description:"The Swift tenant name." env:"SWIFT_TENANTNAME"`
		RegionName string `long:"swift-region-name" description:"The Swift region name." env:"SWIFT_REGIONNAME"`
	} `group:"Swift Options"`

	Docker struct {
		Endpoint string `short:"e" long:"docker-endpoint" description:"The Docker endpoint." env:"DOCKER_ENDPOINT" default:"unix:///var/run/docker.sock"`
	} `group:"Docker Options"`
}

// Conplicity is the main handler struct
type Conplicity struct {
	*docker.Client
	Config   *config
	Hostname string
	Metrics  []string
}

// Setup sets up a Conplicity struct
func (c *Conplicity) Setup(version string) (err error) {
	c.getEnv(version)

	err = c.setupLoglevel()
	util.CheckErr(err, "Failed to setup log level: %v", "panic")

	c.Hostname, err = os.Hostname()
	util.CheckErr(err, "Failed to get hostname: %v", "panic")

	c.Client, err = docker.NewClient(c.Config.Docker.Endpoint, "", nil, nil)
	util.CheckErr(err, "Failed to create Docker client: %v", "panic")

	err = c.pullImage()
	util.CheckErr(err, "Failed to pull image: %v", "panic")

	return
}

func (c *Conplicity) getEnv(version string) (err error) {
	c.Config = &config{}
	parser := flags.NewParser(c.Config, flags.Default)
	if _, err = parser.Parse(); err != nil {
		os.Exit(1)
	}

	if c.Config.Version {
		fmt.Printf("Conplicity v%v\n", version)
		os.Exit(0)
	}

	if c.Config.Manpage {
		var buf bytes.Buffer
		parser.WriteManPage(&buf)
		fmt.Printf(buf.String())
		os.Exit(0)
	}

	sort.Strings(c.Config.VolumesBlacklist)
	return
}

func (c *Conplicity) setupLoglevel() (err error) {
	switch c.Config.Loglevel {
	case "debug":
		log.SetLevel(log.DebugLevel)
	case "info":
		log.SetLevel(log.InfoLevel)
	case "warn":
		log.SetLevel(log.WarnLevel)
	case "error":
		log.SetLevel(log.ErrorLevel)
	case "fatal":
		log.SetLevel(log.FatalLevel)
	case "panic":
		log.SetLevel(log.PanicLevel)
	default:
		errMsg := fmt.Sprintf("Wrong log level '%v'", c.Config.Loglevel)
		err = errors.New(errMsg)
	}

	if c.Config.JSON {
		log.SetFormatter(&log.JSONFormatter{})
	}

	return
}

func (c *Conplicity) pullImage() (err error) {
	if _, _, err = c.ImageInspectWithRaw(context.Background(), c.Config.Image, false); err != nil {
		// TODO: output pull to logs
		log.WithFields(log.Fields{
			"image": c.Config.Image,
		}).Info("Pulling image")
		_, err = c.Client.ImagePull(context.Background(), c.Config.Image, types.ImagePullOptions{})
	} else {
		log.WithFields(log.Fields{
			"image": c.Config.Image,
		}).Debug("Image already pulled, not pulling")
	}

	return
}

// LaunchDuplicity starts a duplicity container with given command and binds
func (c *Conplicity) LaunchDuplicity(cmd []string, binds []string) (state int, stdout string, err error) {
	env := []string{
		"AWS_ACCESS_KEY_ID=" + c.Config.AWS.AccessKeyID,
		"AWS_SECRET_ACCESS_KEY=" + c.Config.AWS.SecretAccessKey,
		"SWIFT_USERNAME=" + c.Config.Swift.Username,
		"SWIFT_PASSWORD=" + c.Config.Swift.Password,
		"SWIFT_AUTHURL=" + c.Config.Swift.AuthURL,
		"SWIFT_TENANTNAME=" + c.Config.Swift.TenantName,
		"SWIFT_REGIONNAME=" + c.Config.Swift.RegionName,
		"SWIFT_AUTHVERSION=2",
	}

	log.WithFields(log.Fields{
		"image":       c.Config.Image,
		"command":     strings.Join(cmd, " "),
		"environment": strings.Join(env, ", "),
	}).Debug("Creating container")

	container, err := c.ContainerCreate(
		context.Background(),
		&container.Config{
			Cmd:          cmd,
			Env:          env,
			Image:        c.Config.Image,
			OpenStdin:    true,
			StdinOnce:    true,
			AttachStdin:  true,
			AttachStdout: true,
			AttachStderr: true,
			Tty:          true,
		},
		&container.HostConfig{
			Binds: binds,
		}, nil, "",
	)
	util.CheckErr(err, "Failed to create container: %v", "fatal")
	defer c.removeContainer(container.ID)

	log.Debugf("Launching 'duplicity %v'...", strings.Join(cmd, " "))
	err = c.ContainerStart(context.Background(), container.ID, types.ContainerStartOptions{})
	util.CheckErr(err, "Failed to start container: %v", "fatal")

	body, err := c.ContainerLogs(context.Background(), container.ID, types.ContainerLogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Details:    true,
		Follow:     true,
	})
	util.CheckErr(err, "Failed to retrieve logs: %v", "error")

	defer body.Close()
	content, err := ioutil.ReadAll(body)
	util.CheckErr(err, "Failed to read logs from response: %v", "error")

	stdout = string(content)

	cont, err := c.ContainerInspect(context.Background(), container.ID)
	util.CheckErr(err, "Failed to inspect container: %v", "error")

	state = cont.State.ExitCode

	log.Debug(stdout)

	return
}

// PushToPrometheus sends metrics to a Prometheus push gateway
func (c *Conplicity) PushToPrometheus() (err error) {
	if len(c.Metrics) == 0 || c.Config.Metrics.PushgatewayURL == "" {
		return
	}

	url := c.Config.Metrics.PushgatewayURL + "/metrics/job/conplicity/instance/" + c.Hostname
	data := strings.Join(c.Metrics, "\n") + "\n"

	log.WithFields(log.Fields{
		"data": data,
		"url":  url,
	}).Debug("Sending metrics to Prometheus Pushgateway")

	req, err := http.NewRequest("PUT", url, bytes.NewBufferString(data))
	req.Header.Set("Content-Type", "text/plain; version=0.0.4")

	client := &http.Client{}
	resp, err := client.Do(req)

	log.WithFields(log.Fields{
		"resp": resp,
	}).Debug("Received Prometheus response")

	return
}

func (c *Conplicity) removeContainer(id string) {
	log.WithFields(log.Fields{
		"container": id,
	}).Infof("Removing container")
	c.ContainerRemove(context.Background(), id, types.ContainerRemoveOptions{
		Force:         true,
		RemoveVolumes: true,
		RemoveLinks:   true,
	})
}
