package tagcontroller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/mfojtik/bugzilla-operator/pkg/operator/bugutil"

	"github.com/eparis/bugzilla"

	"github.com/mfojtik/bugzilla-operator/pkg/operator/config"
	"github.com/mfojtik/bugzilla-operator/pkg/operator/controller"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
)

type TagController struct {
	controller.ControllerContext
	config config.OperatorConfig
}

func NewTagController(ctx controller.ControllerContext, operatorConfig config.OperatorConfig, recorder events.Recorder) factory.Controller {
	c := &TagController{
		ControllerContext: ctx,
		config:            operatorConfig,
	}
	return factory.New().WithSync(c.sync).ResyncEvery(1*time.Hour).ToController("TagController", recorder)
}

func (c *TagController) sync(ctx context.Context, context factory.SyncContext) error {
	client := c.NewBugzillaClient(ctx)
	slackClient := c.SlackClient(ctx)

	result, err := client.Search(getBugsQuery(&c.config, c.config.Components.List()))
	if err != nil {
		return err
	}

	bugsToUpdate := map[int]bugzilla.BugUpdate{}

	for i := range result {
		comments, err := client.GetCachedBugComments(result[i].ID, result[i].LastChangeTime)
		if err != nil {
			context.Recorder().Warningf("GetBugComments", fmt.Sprintf("Failed to get commments for bug %s: %v", result[i].ID, err))
			continue
		}
		if update := c.handleBug(result[i], comments); update != nil {
			bugsToUpdate[result[i].ID] = *update
		}
	}

	tagCounter := 0

	for bugID, update := range bugsToUpdate {
		// remove if we are ready to update bugs
		/*
			if err := client.UpdateBug(bugID, update); err != nil {
				context.Recorder().Warningf("BugUpdateFailed", fmt.Sprintf("Failed to tag bug %s: %v", bugID, err))
				continue
			}
		*/
		slackClient.MessageAdminChannel(fmt.Sprintf("Bug #%s tagged as %s", bugutil.GetBugURL(bugzilla.Bug{ID: bugID}), update.Whiteboard))
		tagCounter++
	}

	return slackClient.MessageAdminChannel(fmt.Sprintf("%d bugs tagged", tagCounter))
}

func (c *TagController) handleBug(bug *bugzilla.Bug, comments []bugzilla.Comment) *bugzilla.BugUpdate {
	// if bug title contains "[sig-" it indicates a CI/test issues
	if strings.Contains(bug.Summary, "[sig-") {
		return tagUpdate("tag-ci", bug.Whiteboard)
	}

	for i := range comments {
		switch {
		case strings.Contains(comments[i].Text, "prow.svc.ci.openshift.org"),
			strings.Contains(comments[i].Text, "storage.googleapis.com/origin-ci-test"),
			strings.Contains(comments[i].Text, "search.ci.openshift.org"):
			return tagUpdate("tag-ci", bug.Whiteboard)
		}
	}

	return nil
}

func tagUpdate(name string, whiteboard string) *bugzilla.BugUpdate {
	return &bugzilla.BugUpdate{
		Whiteboard:  WithKeyword(whiteboard, name),
		MinorUpdate: true,
	}
}

func WithKeyword(wb string, kwd string) string {
	if strings.Contains(wb, kwd) {
		return wb
	}
	return strings.TrimSpace(strings.TrimSpace(wb) + " " + kwd)
}

func getBugsQuery(config *config.OperatorConfig, components []string) bugzilla.Query {
	return bugzilla.Query{
		Classification: []string{"Red Hat"},
		Product:        []string{"OpenShift Container Platform"},
		Status:         []string{"NEW", "ASSIGNED", "POST", "ON_DEV"},
		Component:      components,
		IncludeFields: []string{
			"id",
			"summary",
			"changeddate",
			"whiteboard",
		},
	}
}
