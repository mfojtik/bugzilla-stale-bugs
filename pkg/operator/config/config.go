package config

import (
	"encoding/base64"
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v2"
	"k8s.io/apimachinery/pkg/util/sets"
)

type Credentials struct {
	Username               string `yaml:"username"`
	Password               string `yaml:"password"`
	APIKey                 string `yaml:"apiKey"`
	SlackToken             string `yaml:"slackToken"`
	SlackVerificationToken string `yaml:"slackVerificationToken"`
	BitlyToken             string `yaml:"bitlyToken"`
}

type SharedCluster struct {
	AWSProfile            string
	AWSSecretAccessKey    string
	AWSAccessKeyID        string
	PullSecret            string
	BaseDomain            string
	OpenShiftDevPubSSHKey string
}

type Transition struct {
	From string `yaml:"from"`
	To   string `yaml:"to"`
}

type BugzillaRelease struct {
	CurrentTargetRelease string   `yaml:"currentTargetRelease"`
	TargetReleases       []string `yaml:"targetReleases"`
}

type Group []string

type Component struct {
	// lead should match the bugzilla default assignee the component and will get notifications of new BZs by default.
	Lead string `yaml:"lead"`
	// pm should match with the component product manager. use the email he use on slack.
	ProductManager string `yaml:"pm"`
	// manager match the engineering manager of a team that own this component. use the email he use on slack
	Manager string `yaml:"manager"`
	// developers are not assigned by default, but might be on first comment if autoCommentAssign is true.
	// This can have group:<group-name> references.
	Developers []string `yaml:"developers"`
	// watchers get notified about new bugzillas. If this is empty, the lead is notified.
	// This can have group:<group-name> references.
	Watchers []string `yaml:"watchers"`
}

type AutomaticReport struct {
	// slackChannel is where to post the report to.
	SlackChannel string `yaml:"slackChannel"`

	// crontab like schedules, e.g.:
	//
	//   @every 1s
	//   @hourly
	//   30 * * * *
	//
	// Default is:
	//   CRON_TZ=Europe/Prague 30 9 1-7,16-23 * 2-4
	//   CRON_TZ=America/New_York 30 9 1-7,16-23 * 2-4
	When []string `yaml:"when"`

	// reports are report identifiers, blocker-bugs,closed-bugs by default.
	Reports []string `yaml:"report"`

	// components is the list of components this report is created for. If empty, all components from top-level are used.
	Components []string `yaml:"components"`
}

type OperatorConfig struct {
	Credentials   Credentials   `yaml:"credentials"`
	SharedCluster SharedCluster `yaml:"sharedCluster"`

	StaleBugComment      string `yaml:"staleBugComment"`
	StaleBugCloseComment string `yaml:"staleBugCloseComment"`

	Release BugzillaRelease `yaml:"release"`

	// groups are list of emails or references to other groups with the syntax group:<other-group>.
	Groups     map[string]Group `yaml:"groups"`
	Components ComponentMap     `yaml:"components"`

	// schedules define when reports are created, which contents and sent to which channel.
	Schedules []AutomaticReport `yaml:"schedules"`

	// SlackChannel is a channel where the operator will post reports/etc.
	// Deprecated: SlackChannel is deprecated, use schedules instead.
	SlackChannel      string `yaml:"slackChannel"`
	SlackAdminChannel string `yaml:"slackAdminChannel"`

	// if Bugzilla email and Slack differ, these are the slack emails keyed by bugzilla emails.
	SlackEmails map[string]string `yaml:"slackEmails"`

	// GithubToken is used to query PR's
	GithubToken string `yaml:"githubToken"`

	DisabledControllers []string `yaml:"disabledControllers"`

	CachePath string `yaml:"cachePath"`
}

// Anonymize makes a shallow copy of the config, suitable for dumping in logs (no sensitive data)
func (c *OperatorConfig) Anonymize() OperatorConfig {
	a := *c
	if user := a.Credentials.Username; len(user) > 0 {
		a.Credentials.Username = strings.Repeat("x", len(a.Credentials.DecodedUsername()))
	}
	if password := a.Credentials.Password; len(password) > 0 {
		a.Credentials.Password = strings.Repeat("x", len(a.Credentials.DecodedPassword()))
	}
	if key := a.Credentials.APIKey; len(key) > 0 {
		a.Credentials.APIKey = strings.Repeat("x", len(a.Credentials.DecodedAPIKey()))
	}
	if key := a.Credentials.SlackToken; len(key) > 0 {
		a.Credentials.SlackToken = strings.Repeat("x", len(a.Credentials.DecodedSlackToken()))
	}
	if key := a.Credentials.SlackVerificationToken; len(key) > 0 {
		a.Credentials.SlackVerificationToken = strings.Repeat("x", len(a.Credentials.DecodedSlackVerificationToken()))
	}
	if awsaccesskey := a.SharedCluster.AWSSecretAccessKey; len(awsaccesskey) > 0 {
		a.SharedCluster.AWSSecretAccessKey = strings.Repeat("x", len(a.SharedCluster.DecodedAWSSecretAccessKey()))
	}
	if awskeyid := a.SharedCluster.AWSAccessKeyID; len(awskeyid) > 0 {
		a.SharedCluster.AWSAccessKeyID = strings.Repeat("x", len(a.SharedCluster.DecodedAWSAccessKeyID()))
	}
	if pullSecret := a.SharedCluster.PullSecret; len(pullSecret) > 0 {
		a.SharedCluster.PullSecret = strings.Repeat("x", len(a.SharedCluster.DecodedPullSecret()))
	}

	return a
}

func decode(s string) string {
	if strings.HasPrefix(s, "base64:") {
		data, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(s, "base64:"))
		if err != nil {
			return s
		}
		return string(data)
	}
	return s
}

// DecodedAPIKey return decoded APIKey (in case it was base64 encoded)
func (b Credentials) DecodedAPIKey() string {
	return decode(b.APIKey)
}

// DecodedAPIKey return decoded Password (in case it was base64 encoded)
func (b Credentials) DecodedPassword() string {
	return decode(b.Password)
}

// DecodedAPIKey return decoded Username (in case it was base64 encoded)
func (b Credentials) DecodedUsername() string {
	return decode(b.Username)
}

func (b Credentials) DecodedSlackToken() string {
	return decode(b.SlackToken)
}

func (b Credentials) DecodedSlackVerificationToken() string {
	return decode(b.SlackVerificationToken)
}

func (b SharedCluster) DecodedAWSSecretAccessKey() string {
	return decode(b.AWSSecretAccessKey)
}

func (b SharedCluster) DecodedAWSAccessKeyID() string {
	return decode(b.AWSAccessKeyID)
}

func (b SharedCluster) DecodedPullSecret() string {
	return decode(b.PullSecret)
}

func ExpandGroups(cfg map[string]Group, roots ...string) sets.String {
	users := sets.String{}
	for _, r := range roots {
		users, _ = expandGroup(cfg, r, users, nil)
	}
	return users
}

func expandGroup(cfg map[string]Group, x string, expanded sets.String, seen sets.String) (sets.String, sets.String) {
	if strings.HasPrefix(x, "group:") {
		group := x[6:]
		if seen.Has(group) {
			return expanded, seen
		}
		if seen == nil {
			seen = sets.String{}
		}
		seen = seen.Insert(group)
		for _, y := range cfg[group] {
			expanded, seen = expandGroup(cfg, y, expanded, seen)
		}
		return expanded, seen
	}

	return expanded.Insert(x), seen
}

type ComponentMap map[string]Component

func (cm *ComponentMap) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var x interface{}
	if err := unmarshal(&x); err != nil {
		return err
	}

	m := map[string]Component{}

	if arr, ok := x.([]interface{}); ok {
		for _, a := range arr {
			c, ok := a.(string)
			if !ok {
				return fmt.Errorf("expected a string, got: %v", a)
			}
			m[c] = Component{}
		}
	} else {
		bs, err := yaml.Marshal(x)
		if err != nil {
			return err
		}
		if err := yaml.Unmarshal(bs, &m); err != nil {
			return err
		}
	}

	*cm = m
	return nil
}

func (cm *ComponentMap) List() []string {
	l := make([]string, 0, len(*cm))
	for c := range *cm {
		l = append(l, c)
	}
	sort.Strings(l)
	return l
}

func (cm *ComponentMap) ProductManagerFor(component, fallback string) string {
	for name, c := range *cm {
		if name == component {
			return c.ProductManager
		}
	}
	return fallback
}

func (cm *ComponentMap) ManagerFor(component, fallback string) string {
	for name, c := range *cm {
		if name == component {
			return c.Manager
		}
	}
	return fallback
}
