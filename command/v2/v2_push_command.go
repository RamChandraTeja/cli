package v2

import (
	"os"

	"code.cloudfoundry.org/cli/actor/pushaction"
	"code.cloudfoundry.org/cli/actor/pushaction/manifest"
	"code.cloudfoundry.org/cli/actor/sharedaction"
	"code.cloudfoundry.org/cli/actor/v2action"
	"code.cloudfoundry.org/cli/command"
	"code.cloudfoundry.org/cli/command/flag"
	"code.cloudfoundry.org/cli/command/v2/shared"
	log "github.com/Sirupsen/logrus"
	"github.com/cloudfoundry/noaa/consumer"
)

//go:generate counterfeiter . V2PushActor

type V2PushActor interface {
	Apply(config pushaction.ApplicationConfig) (<-chan pushaction.Event, <-chan pushaction.Warnings, <-chan error)
	ConvertToApplicationConfig(orgGUID string, spaceGUID string, apps []manifest.Application) ([]pushaction.ApplicationConfig, pushaction.Warnings, error)
	MergeAndValidateSettingsAndManifests(cmdSettings pushaction.CommandLineSettings, apps []manifest.Application) ([]manifest.Application, error)
}

type V2PushCommand struct {
	OptionalArgs         flag.AppName                `positional-args:"yes"`
	BuildpackName        string                      `short:"b" description:"Custom buildpack by name (e.g. my-buildpack) or Git URL (e.g. 'https://github.com/cloudfoundry/java-buildpack.git') or Git URL with a branch or tag (e.g. 'https://github.com/cloudfoundry/java-buildpack.git#v3.3.0' for 'v3.3.0' tag). To use built-in buildpacks only, specify 'default' or 'null'"`
	StartupCommand       string                      `short:"c" description:"Startup command, set to null to reset to default start command"`
	Domain               string                      `short:"d" description:"Domain (e.g. example.com)"`
	DockerImage          string                      `long:"docker-image" short:"o" description:"Docker-image to be used (e.g. user/docker-image-name)"`
	PathToManifest       flag.PathWithExistenceCheck `short:"f" description:"Path to manifest"`
	HealthCheckType      flag.HealthCheckType        `long:"health-check-type" short:"u" description:"Application health check type (Default: 'port', 'none' accepted for 'process', 'http' implies endpoint '/')"`
	Hostname             string                      `long:"hostname" short:"n" description:"Hostname (e.g. my-subdomain)"`
	NumInstances         int                         `short:"i" description:"Number of instances"`
	DiskLimit            string                      `short:"k" description:"Disk limit (e.g. 256M, 1024M, 1G)"`
	MemoryLimit          string                      `short:"m" description:"Memory limit (e.g. 256M, 1024M, 1G)"`
	NoHostname           bool                        `long:"no-hostname" description:"Map the root domain to this app"`
	NoManifest           bool                        `long:"no-manifest" description:"Ignore manifest file"`
	NoRoute              bool                        `long:"no-route" description:"Do not map a route to this app and remove routes from previous pushes of this app"`
	NoStart              bool                        `long:"no-start" description:"Do not start an app after pushing"`
	DirectoryPath        flag.PathWithExistenceCheck `short:"p" description:"Path to app directory or to a zip file of the contents of the app directory"`
	RandomRoute          bool                        `long:"random-route" description:"Create a random route for this app"`
	RoutePath            string                      `long:"route-path" description:"Path for the route"`
	Stack                string                      `short:"s" description:"Stack to use (a stack is a pre-built file system, including an operating system, that can run apps)"`
	ApplicationStartTime int                         `short:"t" description:"Time (in seconds) allowed to elapse between starting up an app and the first healthy response from the app"`

	usage               interface{} `usage:"Push a single app (with or without a manifest):\n   CF_NAME v2-push APP_NAME [-b BUILDPACK_NAME] [-c COMMAND] [-d DOMAIN] [-f MANIFEST_PATH] [--docker-image DOCKER_IMAGE]\n   [-i NUM_INSTANCES] [-k DISK] [-m MEMORY] [--hostname HOST] [-p PATH] [-s STACK] [-t TIMEOUT] [-u (process | port | http)] [--route-path ROUTE_PATH]\n   [--no-hostname] [--no-manifest] [--no-route] [--no-start] [--random-route]\n\n   Push multiple apps with a manifest:\n   cf v2-push [-f MANIFEST_PATH]"`
	envCFStagingTimeout interface{} `environmentName:"CF_STAGING_TIMEOUT" environmentDescription:"Max wait time for buildpack staging, in minutes" environmentDefault:"15"`
	envCFStartupTimeout interface{} `environmentName:"CF_STARTUP_TIMEOUT" environmentDescription:"Max wait time for app instance startup, in minutes" environmentDefault:"5"`
	relatedCommands     interface{} `related_commands:"apps, create-app-manifest, logs, ssh, start"`

	UI          command.UI
	Config      command.Config
	SharedActor command.SharedActor
	Actor       V2PushActor
	StartActor  StartActor
	NOAAClient  *consumer.Consumer
}

func (cmd *V2PushCommand) Setup(config command.Config, ui command.UI) error {
	cmd.UI = ui
	cmd.Config = config
	cmd.SharedActor = sharedaction.NewActor()

	ccClient, uaaClient, err := shared.NewClients(config, ui, true)
	if err != nil {
		return err
	}
	v2Actor := v2action.NewActor(ccClient, uaaClient)
	cmd.StartActor = v2Actor
	cmd.Actor = pushaction.NewActor(v2Actor)
	return nil
}

func (cmd V2PushCommand) Execute(args []string) error {
	cmd.UI.DisplayWarning(command.ExperimentalWarning)

	err := cmd.SharedActor.CheckTarget(cmd.Config, true, true)
	if err != nil {
		return shared.HandleError(err)
	}

	log.Info("collating flags")
	cliSettings, err := cmd.GetCommandLineSettings()
	if err != nil {
		log.Errorln("reading flags:", err)
		return shared.HandleError(err)
	}

	//TODO: Read in manifest
	log.Info("merging manifest and command flags")
	manifestApplications, err := cmd.Actor.MergeAndValidateSettingsAndManifests(cliSettings, nil)
	if err != nil {
		log.Errorln("merging manifest:", err)
		return shared.HandleError(err)
	}

	cmd.UI.DisplayText("Getting app info...")

	log.Info("converting manifests to ApplicationConfigs")
	appConfigs, warnings, err := cmd.Actor.ConvertToApplicationConfig(
		cmd.Config.TargetedOrganization().GUID,
		cmd.Config.TargetedSpace().GUID,
		manifestApplications,
	)
	cmd.UI.DisplayWarnings(warnings)
	if err != nil {
		log.Errorln("converting manifest:", err)
		return shared.HandleError(err)
	}

	for _, appConfig := range appConfigs {
		log.Infoln("starting create/update:", appConfig.DesiredApplication.Name)
		eventStream, warningsStream, errorStream := cmd.Actor.Apply(appConfig)
		err := cmd.processApplyStreams(appConfig, eventStream, warningsStream, errorStream)
		if err != nil {
			return shared.HandleError(err)
		}
		//TODO call start / display App
	}

	return nil
}

func (cmd V2PushCommand) GetCommandLineSettings() (pushaction.CommandLineSettings, error) {
	pwd, err := os.Getwd()
	if err != nil {
		return pushaction.CommandLineSettings{}, err
	}

	config := pushaction.CommandLineSettings{
		Name: cmd.OptionalArgs.AppName,
		Path: pwd,
	}

	log.Debugf("%#v", config)
	return config, nil
}

func (cmd V2PushCommand) processApplyStreams(appConfig pushaction.ApplicationConfig, eventStream <-chan pushaction.Event, warningsStream <-chan pushaction.Warnings, errorStream <-chan error) error {
	var eventClosed, warningsClosed, complete bool

	for {
		select {
		case event, ok := <-eventStream:
			if !ok {
				log.Debug("received event stream closed")
				eventClosed = true
				break
			}
			var err error
			complete, err = cmd.processEvent(appConfig, event)
			if err != nil {
				return err
			}
		case warnings, ok := <-warningsStream:
			if !ok {
				log.Debug("received warnings stream closed")
				warningsClosed = true
			}
			cmd.UI.DisplayWarnings(warnings)
		case err, ok := <-errorStream:
			if !ok {
				log.Debug("received error stream closed")
				warningsClosed = true
			}
			return err
		}

		if eventClosed && warningsClosed && complete {
			log.Debug("breaking apply display loop")
			break
		}
	}

	return nil
}

func (cmd V2PushCommand) processEvent(appConfig pushaction.ApplicationConfig, event pushaction.Event) (bool, error) {
	log.Infoln("received apply event:", event)

	switch event {
	case pushaction.ApplicationCreated:
		user, err := cmd.Config.CurrentUser()
		if err != nil {
			return false, err
		}

		cmd.UI.DisplayTextWithFlavor(
			"Creating app {{.AppName}} in org {{.OrgName}} / space {{.SpaceName}} as {{.Username}}...",
			map[string]interface{}{
				"AppName":   appConfig.DesiredApplication.Name,
				"OrgName":   cmd.Config.TargetedOrganization().Name,
				"SpaceName": cmd.Config.TargetedSpace().Name,
				"Username":  user.Name,
			},
		)

	case pushaction.ApplicationUpdated:
		user, err := cmd.Config.CurrentUser()
		if err != nil {
			return false, err
		}

		cmd.UI.DisplayTextWithFlavor(
			"Updating app {{.AppName}} in org {{.OrgName}} / space {{.SpaceName}} as {{.Username}}...",
			map[string]interface{}{
				"AppName":   appConfig.DesiredApplication.Name,
				"OrgName":   cmd.Config.TargetedOrganization().Name,
				"SpaceName": cmd.Config.TargetedSpace().Name,
				"Username":  user.Name,
			},
		)
	case pushaction.RouteCreated:
		cmd.UI.DisplayText("Creating routes...")
	case pushaction.RouteBound:
		cmd.UI.DisplayText("Binding routes...")
	case pushaction.UploadingApplication:
		cmd.UI.DisplayText("Uploading application...")
	case pushaction.UploadComplete:
		cmd.UI.DisplayText("Upload complete")
	case pushaction.Complete:
		return true, nil
	}
	return false, nil
}
