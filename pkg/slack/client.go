package slack

import (
	"fmt"

	"github.com/mfojtik/bugzilla-operator/pkg/operator/config"

	"github.com/slack-go/slack"
)

type ChannelClient interface {
	MessageChannel(message string) error
	MessageAdminChannel(message string) error
	MessageEmail(email, message string) error

	PostMessageChannel(options ...slack.MsgOption) (channelID string, ts string, err error)
	PostMessageAdminChannel(options ...slack.MsgOption) error
	PostMessageEmail(email string, options ...slack.MsgOption) error
	PostFileToChannel(message, filepath string) (*slack.ItemRef, error)
	RemovePinFromChannel(itemRef *slack.ItemRef) error
}
}

type slackClient struct {
	client                *slack.Client
	config                *config.OperatorConfig
	channel, adminChannel string
	debug                 bool
}

func BugzillaToSlackEmail(config *config.OperatorConfig, bzEmail string) string {
	if realEmail, ok := config.SlackEmails[bzEmail]; ok {
		return realEmail
	}
	return bzEmail
}

func SlackEmailToBugzilla(config *config.OperatorConfig, slackEmail string) string {
	for bz, slack := range config.SlackEmails {
		if slackEmail == slack {
			return bz
		}
	}
	return slackEmail
}

func (c *slackClient) MessageChannel(message string) error {
	if c.debug {
		message = fmt.Sprintf("DEBUG CHANNEL #%s: %s", c.channel, message)
	}
	_, _, err := c.PostMessageChannel(slack.MsgOptionText(message, false))
	return err
}

func (c *slackClient) MessageAdminChannel(message string) error {
	if c.debug {
		message = fmt.Sprintf("DEBUG ADMIN #%s: %s", c.adminChannel, message)
	}
	return c.PostMessageAdminChannel(slack.MsgOptionText(message, false))
}

func (c *slackClient) MessageEmail(email, message string) error {
	if c.debug {
		return c.MessageChannel(fmt.Sprintf("DEBUG: %q will receive:\n%s", email, message))
	}
	return c.PostMessageEmail(email, slack.MsgOptionText(message, false))
}

func (c *slackClient) PostMessageChannel(options ...slack.MsgOption) (channelID string, ts string, err error) {
	return c.client.PostMessage(c.channel, options...)
}

func (c *slackClient) PostMessageAdminChannel(options ...slack.MsgOption) error {
	_, _, err := c.client.PostMessage(c.adminChannel, options...)
	return err
}

func (c *slackClient) PostMessageEmail(email string, options ...slack.MsgOption) error {
	user, err := c.client.GetUserByEmail(BugzillaToSlackEmail(c.config, email))
	if err != nil {
		return err
	}
	channel, _, _, err := c.client.OpenConversation(&slack.OpenConversationParameters{
		ReturnIM: true,
		Users:    []string{user.ID},
	})
	if err != nil {
		return err
	}
	_, _, err = c.client.PostMessage(channel.ID, options...)
	return err
}

func (c *slackClient) PostFileToChannel(message string, filepath string) (*slack.ItemRef, error) {
	if c.debug {
		message = fmt.Sprintf("DEBUG CHANNEL #%s: %s", c.channel, message)
	}
	fileParams := slack.FileUploadParameters{
		File: filepath,
		Filename: "kubeconfig",
		InitialComment: message,
		Channels: []string{c.channel},
	}

	file, err := c.client.UploadFile(fileParams)
	if err != nil {
		return nil, err
	}
	itemRef := slack.NewRefToMessage(c.channel, fmt.Sprint(file.Timestamp))
	itemRef.Comment = message
	err = c.client.AddPin(c.channel, slack.ItemRef{Timestamp: fmt.Sprint(file.Timestamp), Channel: c.channel})
	if err != nil {
		return nil, err
	}
	return &itemRef, nil
}

func (c *slackClient) RemovePinFromChannel(itemRef *slack.ItemRef) error {
	err := c.client.RemovePin(c.channel, *itemRef)
	if err != nil {
		return err
	}
	return nil
}

func NewChannelClient(client *slack.Client, config *config.OperatorConfig, channel, adminChannel string, debug bool) ChannelClient {
	c := &slackClient{
		channel:      channel,
		config:       config,
		adminChannel: adminChannel,
		client:       client,
		debug:        debug,
	}
	return c
}
