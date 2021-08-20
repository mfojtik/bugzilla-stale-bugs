package operator

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/andygrunwald/go-jira"
	"github.com/eparis/bugzilla"
	"github.com/google/go-github/v33/github"
	"github.com/openshift/library-go/pkg/controller/factory"
	slackgo "github.com/slack-go/slack"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog"

	"github.com/mfojtik/bugzilla-operator/pkg/cache"
	"github.com/mfojtik/bugzilla-operator/pkg/operator/closecontroller"
	"github.com/mfojtik/bugzilla-operator/pkg/operator/config"
	"github.com/mfojtik/bugzilla-operator/pkg/operator/controller"
	"github.com/mfojtik/bugzilla-operator/pkg/operator/escalationcontroller"
	"github.com/mfojtik/bugzilla-operator/pkg/operator/firstteamcommentcontroller"
	"github.com/mfojtik/bugzilla-operator/pkg/operator/ideas"
	"github.com/mfojtik/bugzilla-operator/pkg/operator/needinfocontroller"
	"github.com/mfojtik/bugzilla-operator/pkg/operator/reporters/blockers"
	"github.com/mfojtik/bugzilla-operator/pkg/operator/reporters/closed"
	"github.com/mfojtik/bugzilla-operator/pkg/operator/reporters/escalation"
	"github.com/mfojtik/bugzilla-operator/pkg/operator/reporters/incoming"
	newreporter "github.com/mfojtik/bugzilla-operator/pkg/operator/reporters/new"
	"github.com/mfojtik/bugzilla-operator/pkg/operator/reporters/reassign"
	"github.com/mfojtik/bugzilla-operator/pkg/operator/reporters/stalepost"
	"github.com/mfojtik/bugzilla-operator/pkg/operator/reporters/upcomingsprint"
	"github.com/mfojtik/bugzilla-operator/pkg/operator/resetcontroller"
	"github.com/mfojtik/bugzilla-operator/pkg/operator/sharedclustercontroller"
	"github.com/mfojtik/bugzilla-operator/pkg/operator/stalecontroller"
	"github.com/mfojtik/bugzilla-operator/pkg/operator/tagcontroller"
	"github.com/mfojtik/bugzilla-operator/pkg/operator/unfurl"
	"github.com/mfojtik/bugzilla-operator/pkg/slack"
	"github.com/mfojtik/bugzilla-operator/pkg/slacker"
)

const bugzillaEndpoint = "https://bugzilla.redhat.com"

func Run(ctx context.Context, cfg config.OperatorConfig) error {
	if len(cfg.CachePath) > 0 {
		cache.Open(cfg.CachePath)
	}
	defer cache.Close()

	slackClient := slackgo.New(cfg.Credentials.DecodedSlackToken(), slackgo.OptionDebug(true))

	// This slack client is used for debugging
	slackDebugClient := slack.NewChannelClient(slackClient, &cfg, cfg.SlackAdminChannel, cfg.SlackAdminChannel, true)

	// This slack client posts only to the admin channel
	slackAdminClient := slack.NewChannelClient(slackClient, &cfg, cfg.SlackAdminChannel, cfg.SlackAdminChannel, false)

	recorder := slack.NewRecorder(slackAdminClient, "BugzillaOperator")
	defer func() {
		recorder.Warningf("Shutdown", ":crossed_fingers: *The bot is shutting down*")
	}()

	slackerInstance := slacker.NewSlacker(slackClient, slacker.Options{
		ListenAddress:     "0.0.0.0:3000",
		VerificationToken: cfg.Credentials.DecodedSlackVerificationToken(),
	})
	slackerInstance.Command("say <message>", &slacker.CommandDefinition{
		Description: "Say something.",
		Handler: func(req slacker.Request, w slacker.ResponseWriter) {
			msg := req.StringParam("message", "")
			w.Reply(msg, slacker.WithThreadReply(true))
		},
	})
	slackerInstance.DefaultCommand(func(req slacker.Request, w slacker.ResponseWriter) {
		w.Reply("Unknown command", slacker.WithThreadReply(true))
	})

	recorder.Eventf("OperatorRestarted", "Bugzilla Operator Started\n")

	// Setup unfurl handlers
	if err := unfurl.UnfurlBugzillaLinks(slackerInstance, slackClient, newAnonymousBugzillaClient(slackAdminClient)(false)); err != nil {
		return err
	}
	if err := unfurl.UnfurlGithubLinks(slackerInstance, slackClient, github.NewClient(nil)); err != nil {
		return err
	}
	jiraClient, err := jira.NewClient(nil, "https://issues.redhat.com/")
	if err != nil {
		return err
	}
	if err := unfurl.UnfurlJiraLinks(slackerInstance, slackClient, jiraClient); err != nil {
		return err
	}

	kubeConfig, err := rest.InClusterConfig()
	if err != nil {
		return err
	}
	kubeClient, err := kubernetes.NewForConfig(kubeConfig)
	if err != nil {
		return err
	}
	cmClient := kubeClient.CoreV1().ConfigMaps(os.Getenv("POD_NAMESPACE"))

	controllerContext := controller.NewControllerContext(newBugzillaClient(&cfg, slackDebugClient), slackAdminClient, slackDebugClient, slackerInstance, cmClient)
	controllers := map[string]factory.Controller{
		"shared-cluster":     sharedclustercontroller.NewSharedClusterController(controllerContext, cfg, recorder),
		"stale":              stalecontroller.NewStaleController(controllerContext, cfg, recorder),
		"stale-reset":        resetcontroller.NewResetStaleController(controllerContext, cfg, recorder),
		"tag":                tagcontroller.NewTagController(controllerContext, cfg, recorder),
		"close-stale":        closecontroller.NewCloseStaleController(controllerContext, cfg, recorder),
		"first-team-comment": firstteamcommentcontroller.NewFirstTeamCommentController(controllerContext, cfg, slackClient, recorder),
		"needinfo":           needinfocontroller.NewNeedInfoController(controllerContext, cfg, recorder),
		"urgent":             escalationcontroller.NewEscalationController(controllerContext, cfg, recorder),
	}

	ideasController := ideas.New(controllerContext)
	ideasController.AddCommands(slackerInstance)

	newScheduledReport := func(name string, ctx controller.ControllerContext, components, when []string) factory.Controller {
		switch name {
		case "blocker-bugs":
			return blockers.NewChannelBlockersReporter(ctx, components, when, cfg, recorder)
		case "user-triage-bugs":
			return blockers.NewToTriageReminder(ctx, components, when, cfg, recorder)
		case "user-urgent-bugs":
			return blockers.NewUrgentReminder(ctx, components, when, cfg, recorder)
		case "user-blocker-bugs":
			return blockers.NewBlockerReminder(ctx, components, when, cfg, recorder)
		case "incoming-bugs":
			return incoming.NewIncomingReporter(ctx, components, when, cfg, recorder)
		case "incoming-stats":
			return incoming.NewIncomingStatsReporter(ctx, components, when, recorder)
		case "moved-bugs":
			return reassign.NewReassignReporter(ctx, components, when, cfg, recorder)
		case "closed-bugs":
			return closed.NewClosedReporter(ctx, components, when, cfg, recorder)
		case "upcoming-sprint":
			return upcomingsprint.NewUpcomingSprintReporter(controllerContext, components, when, cfg, recorder)
		case "escalations":
			return escalation.NewEscalationReporter(ctx, components, when, cfg, recorder)
		case "post-stale":
			return stalepost.NewStalePostReporter(ctx, components, when, cfg, recorder)
		case "new-bugs":
			return newreporter.NewNewBugReporter(ctx, components, when, cfg, slackClient, recorder)
		default:
			return nil
		}
	}
	var scheduledReports []factory.Controller
	reportComponents := map[string][]string{}
	for _, ar := range cfg.Schedules {
		slackChannelClient := slack.NewChannelClient(slackClient, &cfg, ar.SlackChannel, cfg.SlackAdminChannel, false)
		reporterContext := controller.NewControllerContext(newBugzillaClient(&cfg, slackDebugClient), slackChannelClient, slackDebugClient, slackerInstance, cmClient)
		for _, r := range ar.Reports {
			if c := newScheduledReport(r, reporterContext, ar.Components, ar.When); c != nil {
				scheduledReports = append(scheduledReports, c)
				reportComponents[r] = append(reportComponents[r], ar.Components...)
			}
		}
	}
	triggerableReports := map[string]factory.Controller{}
	scheduledReportNames := sets.NewString()
	for r, comps := range reportComponents {
		scheduledReportNames.Insert(r)
		triggerableReports[r] = newScheduledReport(r, controllerContext, sets.NewString(comps...).List(), nil)
	}

	controllerNames := sets.NewString()
	for n := range controllers {
		controllerNames.Insert(n)
	}

	// allow to manually trigger a controller to run out of its normal schedule
	runJob := func(debug bool) func(req slacker.Request, w slacker.ResponseWriter) {
		return func(req slacker.Request, w slacker.ResponseWriter) {
			job := req.StringParam("job", "")

			c, ok := controllers[job]
			if !ok {
				c, ok = triggerableReports[job]
				if !ok {
					w.Reply(fmt.Sprintf("Unknown job %q", job), slacker.WithThreadReply(true))
					return
				}
			}

			ctx := ctx // shadow global ctx
			if debug {
				ctx = context.WithValue(ctx, "debug", debug)
			}

			startTime := time.Now()
			_, _, _, err := w.Client().SendMessage(req.Event().Channel,
				slackgo.MsgOptionPostEphemeral(req.Event().User),
				slackgo.MsgOptionText(fmt.Sprintf("Triggering job %q", job), false))
			if err != nil {
				klog.Error(err)
			}

			if err := c.Sync(ctx, factory.NewSyncContext(job, recorder)); err != nil {
				recorder.Warningf("ReportError", "Job reported error: %v", err)
				return
			}

			_, _, _, err = w.Client().SendMessage(req.Event().Channel,
				slackgo.MsgOptionPostEphemeral(req.Event().User),
				slackgo.MsgOptionText(fmt.Sprintf("Finished job %q after %v", job, time.Since(startTime)), false))
			if err != nil {
				klog.Error(err)
			}
		}
	}
	slackerInstance.Command("admin trigger <job>", &slacker.CommandDefinition{
		Description: fmt.Sprintf("Trigger a job to run: %s", strings.Join(append(controllerNames.List(), scheduledReportNames.List()...), ", ")),
		Handler:     auth(cfg, runJob(false), "group:admins"),
	})
	slackerInstance.Command("admin debug <job>", &slacker.CommandDefinition{
		Description: fmt.Sprintf("Trigger a job to run in debug mode: %s", strings.Join(append(controllerNames.List(), scheduledReportNames.List()...), ", ")),
		Handler:     auth(cfg, runJob(true), "group:admins"),
	})
	slackerInstance.Command("admin interact", &slacker.CommandDefinition{
		Description: "Trigger some interaction",
		Handler: auth(cfg, func(req slacker.Request, w slacker.ResponseWriter) {
			w.Client().PostMessage(
				req.Event().Channel,
				slackgo.MsgOptionBlocks(
					slackgo.NewSectionBlock(slackgo.NewTextBlockObject("mrkdwn", "Some interaction.", false, false), nil, nil),
					slackgo.NewActionBlock("admin-interact",
						slackgo.NewButtonBlockElement("btn", "some value", slackgo.NewTextBlockObject("plain_text", "Create bug :bugzilla:", true, false)).WithStyle(slackgo.StylePrimary),
					),
				),
			)
		}, "group:admins"),
		Init: func() {
			slackerInstance.SubscribeBlockAction("admin-interact", func(msg *slackgo.Container, u *slackgo.User, a *slackgo.BlockAction) {
				slackClient.SendMessage(
					msg.ChannelID,
					slackgo.MsgOptionText(fmt.Sprintf("%s clicked button with value %q", u.Name, a.Value), false),
				)

				if _, _, _, err := slackClient.UpdateMessage(
					msg.ChannelID,
					msg.MessageTs,
					slackgo.MsgOptionBlocks(
						slackgo.NewSectionBlock(slackgo.NewTextBlockObject("mrkdwn", "~Some interaction.~ Button clicked.", false, false), nil, nil),
					),
				); err != nil {
					slackClient.SendMessage(
						msg.ChannelID,
						slackgo.MsgOptionText(fmt.Sprintf("Failed to update message: %v", err), false),
					)
					klog.Errorf("Failed to update message: %v", err)
				}
			})
		},
	})
	slackerInstance.Command("report <job>", &slacker.CommandDefinition{
		Description: fmt.Sprintf("Run a report and print result here: %s", strings.Join(scheduledReportNames.List(), ", ")),
		Handler: func(req slacker.Request, w slacker.ResponseWriter) {
			job := req.StringParam("job", "")
			reports := map[string]func(ctx context.Context, client cache.BugzillaClient) (string, error){
				"blocker-bugs": func(ctx context.Context, client cache.BugzillaClient) (string, error) {
					// TODO: restrict components to one team
					report, _, _, err := blockers.Report(ctx, client, recorder, &cfg, cfg.Components.List())
					return report, err
				},
				"closed-bugs": func(ctx context.Context, client cache.BugzillaClient) (string, error) {
					// TODO: restrict components to one team
					return closed.Report(ctx, client, recorder, &cfg, cfg.Components.List())
				},
				"incoming-bugs": func(ctx context.Context, client cache.BugzillaClient) (string, error) {
					// TODO: restrict components to one team
					report, _, _, err := incoming.Report(ctx, client, recorder, cfg.Components.List())
					return report, err
				},
				"incoming-stats": func(ctx context.Context, client cache.BugzillaClient) (string, error) {
					// TODO: restrict components to one team
					return incoming.ReportStats(ctx, controllerContext, recorder)
				},
				"moved-bugs": func(ctx context.Context, client cache.BugzillaClient) (string, error) {
					// TODO: restrict components to one team
					return reassign.Report(ctx, controllerContext, recorder, &cfg)
				},
				"upcoming-sprint": func(ctx context.Context, client cache.BugzillaClient) (string, error) {
					// TODO: restrict components to one team
					return upcomingsprint.Report(ctx, client, recorder, cfg.Components.List())
				},
				"escalations": func(ctx context.Context, client cache.BugzillaClient) (string, error) {
					// TODO: restrict components to one team
					report, _, err := escalation.Report(ctx, client, nil, recorder, &cfg, cfg.Components.List())
					return report, err
				},
				"post-stale": func(ctx context.Context, client cache.BugzillaClient) (string, error) {
					return stalepost.Report(ctx, client, cfg.Components.List(), &cfg)
				},
				"post-stale-urgent": func(ctx context.Context, client cache.BugzillaClient) (string, error) {
					urgentCtx := context.WithValue(ctx, "urgent", true)
					return stalepost.Report(urgentCtx, client, cfg.Components.List(), &cfg)
				},
				"new-bugs": func(ctx context.Context, client cache.BugzillaClient) (string, error) {
					return newreporter.Report(ctx, client, cfg.Components.List())
				},

				// don't forget to also add new reports above in the trigger command
			}

			report, ok := reports[job]
			if !ok {
				w.Reply(fmt.Sprintf("Unknown report %q", job), slacker.WithThreadReply(true))
				return
			}

			_, _, _, err := w.Client().SendMessage(req.Event().Channel,
				slackgo.MsgOptionPostEphemeral(req.Event().User),
				slackgo.MsgOptionText(fmt.Sprintf("Running job %q. This might take some seconds.", job), false))
			if err != nil {
				klog.Error(err)
			}

			reply, err := report(context.TODO(), newBugzillaClient(&cfg, slackDebugClient)(true)) // report should never write anything to BZ
			if err != nil {
				_, _, _, err := w.Client().SendMessage(req.Event().Channel,
					slackgo.MsgOptionPostEphemeral(req.Event().User),
					slackgo.MsgOptionText(fmt.Sprintf("Error running report %v: %v", job, err), false))
				if err != nil {
					klog.Error(err)
				}
			} else {
				w.Reply(reply, slacker.WithThreadReply(true))
			}
		},
	})

	seen := []string{}
	disabled := sets.NewString(cfg.DisabledControllers...)
	var all []factory.Controller
	for _, c := range controllers {
		all = append(all, c)
	}
	for _, c := range scheduledReports {
		all = append(all, c)
	}
	for _, c := range all {
		seen = append(seen, c.Name())
		if disabled.Has(c.Name()) {
			continue
		}
		go c.Run(ctx, 1)
	}

	go slackerInstance.Run(ctx)

	// sanity check list of disabled controllers
	unknown := disabled.Difference(sets.NewString(seen...))
	if unknown.Len() > 0 {
		msg := fmt.Sprintf("Unknown disabled controllers in config: %v", unknown.List())
		klog.Warning(msg)
		slackAdminClient.MessageAdminChannel(msg)
	}

	<-ctx.Done()

	return nil
}

func newBugzillaClient(cfg *config.OperatorConfig, slackDebugClient slack.ChannelClient) func(debug bool) cache.BugzillaClient {
	return func(debug bool) cache.BugzillaClient {
		c := cache.NewCachedBugzillaClient(WithSearchLogging(bugzilla.NewClient(func() []byte {
			return []byte(cfg.Credentials.DecodedAPIKey())
		}, bugzillaEndpoint).WithCGIClient(cfg.Credentials.DecodedUsername(), cfg.Credentials.DecodedPassword())))
		if debug {
			return &loggingReadOnlyClient{delegate: c, slackLoggingClient: slackDebugClient}
		}
		return c
	}
}

func newAnonymousBugzillaClient(slackDebugClient slack.ChannelClient) func(debug bool) cache.BugzillaClient {
	return func(debug bool) cache.BugzillaClient {
		c := cache.NewCachedBugzillaClient(WithSearchLogging(bugzilla.NewClient(func() []byte {
			return nil
		}, bugzillaEndpoint)), cache.CustomCachePrefix("anonymous"))
		if debug {
			return &loggingReadOnlyClient{delegate: c, slackLoggingClient: slackDebugClient}
		}
		return c
	}
}

func WithSearchLogging(client bugzilla.Client) bugzilla.Client {
	return searchLoggingClient{client}
}

type searchLoggingClient struct{ bugzilla.Client }

var searchCount int64

func (c searchLoggingClient) Search(query bugzilla.Query) ([]*bugzilla.Bug, error) {
	url, _ := url.Parse("https://bugzilla.redhat.com/rest/bug")
	url.RawQuery = query.Values().Encode()

	no := atomic.AddInt64(&searchCount, 1)

	klog.Infof("Searching [%d]: %s", no, url.String())
	start := time.Now()
	result, err := c.Client.Search(query)
	if err != nil {
		return nil, err
	}

	klog.Infof("Search [%d] returned %d result after %v", no, len(result), time.Since(start))
	return result, err
}
