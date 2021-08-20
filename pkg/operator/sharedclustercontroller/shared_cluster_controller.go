package sharedclustercontroller

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/slack-go/slack"
	errorutil "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/klog"

	"github.com/mfojtik/bugzilla-operator/pkg/operator/config"
	"github.com/mfojtik/bugzilla-operator/pkg/operator/controller"
)

var (
	clustersDir                                       = "/cache/groupbclusters"
	cutoff                                            = 45 * time.Hour
	kc                                                = "kubeconfig"
	clustersDestroyed                                 []string
	clusterCreated, clusterDestroyed, tooManyClusters bool
	clusterVersionCmd                                 = exec.Command("oc", "get", "clusterversion")
	createClusterCmd                                  = exec.Command("create-cluster")
	destroyClusterCmd                                 = exec.Command("destroy-cluster")
)

type SharedClusterController struct {
	controller.ControllerContext
	operatorConfig        config.OperatorConfig
	kubeadminPW           string
	currentClusterVersion string
	pinnedItemRef         *slack.ItemRef
	clustersToDelete         map[string]string
}

func NewSharedClusterController(ctx controller.ControllerContext, operatorConfig config.OperatorConfig, recorder events.Recorder) factory.Controller {
	c := &SharedClusterController{ctx, operatorConfig, "", "", nil, make(map[string]string)}
	return factory.New().WithSync(c.sync).ResyncEvery(1*time.Hour).ToController("SharedClusterController", recorder)
}

func (c *SharedClusterController) sync(ctx context.Context, syncCtx factory.SyncContext) error {
	var (
		currentClusters []string
		errors          []error
	)
	cfg := c.operatorConfig.SharedCluster
	// set env variables for install, tear-down
	// could instead write an env file and source it but go w/ this for now
	os.Setenv("AWS_PROFILE", cfg.AWSProfile)
	os.Setenv("AWS_SECRET_ACCESS_KEY", cfg.DecodedAWSSecretAccessKey())
	os.Setenv("AWS_ACCESS_KEY_ID", cfg.DecodedAWSAccessKeyID())
	// need path/pull-secret for 'oc adm release extract' in both install, destroy
	if err := ioutil.WriteFile(filepath.Join(clustersDir, "pullsecret"), []byte(cfg.DecodedPullSecret()), 0755); err != nil {
		return err
	}
	os.Setenv("PS_PATH", filepath.Join(clustersDir, "pullsecret"))
	err := os.MkdirAll(clustersDir, os.ModePerm)
	if err != nil && !os.IsExist(err) {
		return err
	}

	slackClient := c.SlackClient(ctx)
	d := time.Now()
	newClusterName := fmt.Sprintf("groupb%02d%02d%02d", d.Month(), d.Day(), d.Hour())
	clusters, err := ioutil.ReadDir(clustersDir)
	if err != nil {
		errors = append(errors, err)
	}
	for _, d := range clusters {
		if d.IsDir() {
			currentClusters = append(currentClusters, d.Name())
		}
	}
	numClusters := len(currentClusters)
	switch numClusters {
	case 0:
		err := c.createCluster(newClusterName)
		if err != nil {
			errors = append(errors, err)
		}
		err = c.getKubeadmin(newClusterName)
		if err != nil {
			errors = append(errors, err)
		}
		clusterCreated = true
	case 1:
		// find out if healthy, calculate age
		name := currentClusters[0]
		clusterAuthDir := filepath.Join(clustersDir, name, "auth")
		files, err := ioutil.ReadDir(clusterAuthDir)
		if err != nil || len(files) != 2 {
			errors = append(errors, fmt.Errorf("error reading clusterAuthDir or unexpected files: %v", err))
		}
		kubeconfig := filepath.Join(clusterAuthDir, "kubeconfig")
		for _, f := range files {
			// kubeadmin-password won't be modified once created so use that to calculate age
			if f.Name() == "kubeadmin-password" {
				err := c.minimumHealthCheck(name, kubeconfig)
				if err != nil {
					errors = append(errors, err)
					c.clustersToDelete[name] = kc
				}
				now := time.Now()
				if diff := now.Sub(f.ModTime()); diff > cutoff {
					if _, ok := c.clustersToDelete[name]; !ok {
						c.clustersToDelete[name] = kc
					}
				}
			}
		}
		if _, ok := c.clustersToDelete[name]; ok {
			// create first, so there's no time w/out cluster
			err := c.createCluster(newClusterName)
			if err != nil {
				errors = append(errors, err)
			}
			err = c.getKubeadmin(newClusterName)
			if err != nil {
				errors = append(errors, err)
			}
			clusterCreated = true

			os.Setenv("EXPIRE_CLUSTER", name)
			expireClusterDir := filepath.Join(clustersDir, name)
			os.Setenv("EXPIRE_CLUSTER_DIR", expireClusterDir)
			err = destroyClusterCmd.Run()
			if err != nil {
				errors = append(errors, err)
			}
			clustersDestroyed = append(clustersDestroyed, name)
		}
	case 2:
		// check the age/health, then destroy the oldest
		// there should not be more than 1, so this should not happen
		// if there are 2, delete the oldest
		nowTime := time.Now()
		largestDiff := nowTime.Sub(time.Now())
		var oldestCluster string
		var kubeconfig string
		for _, d := range currentClusters {
			info, err := os.Stat(filepath.Join(clustersDir, d))
			if err != nil {
				errors = append(errors, err)
			}
			if diff := nowTime.Sub(info.ModTime()); diff > largestDiff {
				largestDiff = diff
				oldestCluster = d
			}
			kubeconfig = filepath.Join(clustersDir, d, "auth", "kubeconfig")
			err = c.minimumHealthCheck(d, kubeconfig)
			if err != nil {
				klog.Infof("will tear down %s, error getting clusterversion: %v", d, err)
				c.clustersToDelete[d] = kc
			}
		}
		if _, ok := c.clustersToDelete[oldestCluster]; !ok {
			c.clustersToDelete[oldestCluster] = kc
		}
		for clustername := range c.clustersToDelete {
			os.Setenv("EXPIRE_CLUSTER", clustername)
			err := destroyClusterCmd.Run()
			if err != nil {
				errors = append(errors, err)
			}
			clustersDestroyed = append(clustersDestroyed, clustername)
		}
	default:
		tooManyClusters = true
		errors = append(errors, fmt.Errorf("unexpected number of clusters, should never be more than 2, currently %d cluster directories are in %s", numClusters, clustersDir))
	}
	// Notify admin
	previousPin := c.pinnedItemRef
	if clusterCreated {
		slackClient.MessageAdminChannel(fmt.Sprintf("%s cluster created: files located at %s", newClusterName, filepath.Join(clustersDir, newClusterName)))
		msg := fmt.Sprintf("%s: kubeadmin password: %s", c.currentClusterVersion, c.kubeadminPW)
		itemRef, err := slackClient.PostFileToChannel(msg, filepath.Join(clustersDir, newClusterName, "auth", "kubeconfig"))
		if err != nil {
			errors = append(errors, err)
		}
		c.pinnedItemRef = itemRef
	}
	if len(clustersDestroyed) > 0 {
		if previousPin != c.pinnedItemRef {
			err := slackClient.RemovePinFromChannel(previousPin)
			if err != nil {
				errors = append(errors, err)
			}
		}
		for _, d := range clustersDestroyed {
			slackClient.MessageAdminChannel(fmt.Sprintf("%s cluster torn down successfully.", d))
		}
	}
	if tooManyClusters {
		slackClient.MessageAdminChannel(fmt.Sprintf("unexpected number of clusters found in %s, more than 2 cluster directories found", clustersDir))
	}

	return errorutil.NewAggregate(errors)
}

func (c *SharedClusterController) createCluster(name string) error {
	cfg := c.operatorConfig.SharedCluster
	clusterDir := filepath.Join(clustersDir, name)
	os.Setenv("CLUSTER_NAME", name)
	os.Setenv("CLUSTER_DIR", clusterDir)
	os.Setenv("BASE_DOMAIN", cfg.BaseDomain)
	os.Setenv("PUB_SSH_KEY", cfg.OpenShiftDevPubSSHKey)
	err := os.Mkdir(clusterDir, 0755)
	err = createClusterCmd.Run()
	if err != nil {
		return err
	}
	if _, err = os.Stat(filepath.Join(clustersDir, name)); os.IsNotExist(err) {
		return fmt.Errorf("new cluster %s is missing cluster directory", name)
	}
	authDir := filepath.Join(clustersDir, name, "auth")
	files, err := ioutil.ReadDir(authDir)
	if err != nil || len(files) != 2 {
		return fmt.Errorf("error reading clusterAuthDir or unexpected files: %v", err)
	}
	return nil
}

func (c *SharedClusterController) minimumHealthCheck(name, kubeconfig string) error {
	// very minimun cluster health check
	latestci := os.Getenv("LATEST_CI")
	os.Setenv("KUBECONFIG", kubeconfig)
	out, err := clusterVersionCmd.Output()
	if err != nil {
		c.clustersToDelete[name] = kc
	}
	healthyString := fmt.Sprintf("Cluster version is %s", latestci)
	if !strings.Contains(string(out), healthyString) {
		if _, ok := c.clustersToDelete[name]; !ok {
			c.clustersToDelete[name] = kc
		}
	}
	c.currentClusterVersion = healthyString
	return nil
}

func (c *SharedClusterController) getKubeadmin(name string) error {
	pwFile := filepath.Join(clustersDir, name, "auth", "kubeadmin-password")
	pw, err := ioutil.ReadFile(pwFile)
	if err != nil {
		return err
	}
	c.kubeadminPW = string(pw)
	return nil
}
