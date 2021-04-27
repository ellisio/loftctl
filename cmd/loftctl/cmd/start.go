package cmd

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	jsonpatch "github.com/evanphx/json-patch"
	"github.com/loft-sh/loftctl/pkg/upgrade"
	"github.com/pkg/errors"
	"io/ioutil"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"regexp"
	"strings"
	"time"

	loftclientset "github.com/loft-sh/api/pkg/client/clientset_generated/clientset"
	"github.com/loft-sh/loftctl/cmd/loftctl/flags"
	"github.com/loft-sh/loftctl/pkg/client"
	"github.com/loft-sh/loftctl/pkg/log"
	"github.com/loft-sh/loftctl/pkg/survey"
	"github.com/mgutz/ansi"
	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/kube-aggregator/pkg/client/clientset_generated/clientset"
)

var emailRegex = regexp.MustCompile("^[^@]+@[^\\.]+\\..+$")

// StartCmd holds the cmd flags
type StartCmd struct {
	*flags.GlobalFlags

	LocalPort        string
	Reset            bool
	Version          string
	Namespace        string
	Password         string

	Log log.Logger
}

// NewStartCmd creates a new command
func NewStartCmd(globalFlags *flags.GlobalFlags) *cobra.Command {
	cmd := &StartCmd{
		GlobalFlags: globalFlags,
		Log:         log.GetInstance(),
	}

	startCmd := &cobra.Command{
		Use:   "start",
		Short: "Start a loft instance and connect via port-forwarding",
		Long: `
#######################################################
###################### loft start #####################
#######################################################
Starts a loft instance in your Kubernetes cluster and
then establishes a port-forwarding connection.

Please make sure you meet the following requirements 
before running this command:

1. Current kube-context has admin access to the cluster
2. Helm v3 must be installed
3. kubectl must be installed

#######################################################
	`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cobraCmd *cobra.Command, args []string) error {
			// Check for newer version
			upgrade.PrintNewerVersionWarning()

			return cmd.Run(cobraCmd, args)
		},
	}

	startCmd.Flags().StringVar(&cmd.Namespace, "namespace", "loft", "The namespace to install loft into")
	startCmd.Flags().StringVar(&cmd.LocalPort, "local-port", "9898", "The local port to bind to if using port-forwarding")
	startCmd.Flags().StringVar(&cmd.Password, "password", "", "The password to use for the admin account. (If empty this will be the namespace UID)")
	startCmd.Flags().StringVar(&cmd.Version, "version", "", "The loft version to install")
	startCmd.Flags().BoolVar(&cmd.Reset, "reset", false, "If true, an existing loft instance will be deleted before installing loft")
	return startCmd
}

// Run executes the functionality "loft start"
func (cmd *StartCmd) Run(cobraCmd *cobra.Command, args []string) error {
	loader, err := client.NewClientFromPath(cmd.Config)
	if err != nil {
		return err
	}
	loftConfig := loader.Config()

	// first load the kube config
	kubeClientConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{})

	// load the raw config
	kubeConfig, err := kubeClientConfig.RawConfig()
	if err != nil {
		return fmt.Errorf("there is an error loading your current kube config (%v), please make sure you have access to a kubernetes cluster and the command `kubectl get namespaces` is working", err)
	}

	// we switch the context to the install config
	contextToLoad := kubeConfig.CurrentContext
	if loftConfig.LastInstallContext != "" && loftConfig.LastInstallContext != contextToLoad {
		contextToLoad, err = cmd.Log.Question(&survey.QuestionOptions{
			Question:     "Seems like you try to use 'loft start' with a different kubernetes context than before. Please choose which kubernetes context you want to use",
			DefaultValue: contextToLoad,
			Options:      []string{contextToLoad, loftConfig.LastInstallContext},
		})
		if err != nil {
			return err
		}
	}

	loftConfig.LastInstallContext = contextToLoad
	_ = loader.Save()

	// kube client config
	kubeClientConfig = clientcmd.NewNonInteractiveClientConfig(kubeConfig, contextToLoad, &clientcmd.ConfigOverrides{}, clientcmd.NewDefaultClientConfigLoadingRules())

	// test for helm and kubectl
	_, err = exec.LookPath("helm")
	if err != nil {
		return fmt.Errorf("Seems like helm is not installed. Helm is required for the installation of loft. Please visit https://helm.sh/docs/intro/install/ for install instructions.")
	}

	output, err := exec.Command("helm", "version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("Seems like there are issues with your helm client: \n\n%s", output)
	}

	_, err = exec.LookPath("kubectl")
	if err != nil {
		return fmt.Errorf("Seems like kubectl is not installed. Kubectl is required for the installation of loft. Please visit https://kubernetes.io/docs/tasks/tools/install-kubectl/ for install instructions.")
	}

	output, err = exec.Command("kubectl", "version", "--context", contextToLoad).CombinedOutput()
	if err != nil {
		return fmt.Errorf("Seems like kubectl cannot connect to your Kubernetes cluster: \n\n%s", output)
	}

	restConfig, err := kubeClientConfig.ClientConfig()
	if err != nil {
		return fmt.Errorf("There is an error loading your current kube config (%v), please make sure you have access to a kubernetes cluster and the command `kubectl get namespaces` is working.", err)
	}
	kubeClient, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("There is an error loading your current kube config (%v), please make sure you have access to a kubernetes cluster and the command `kubectl get namespaces` is working.", err)
	}

	// Check if cluster has RBAC correctly configured
	_, err = kubeClient.RbacV1().ClusterRoles().Get(context.Background(), "cluster-admin", metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("Error retrieving cluster role 'cluster-admin': %v. Please make sure RBAC is correctly configured in your cluster", err)
	}

	// Is already installed?
	isInstalled, err := cmd.isLoftAlreadyInstalled(kubeClient)
	if err != nil {
		return err
	} else if isInstalled {
		if cmd.Reset {
			cmd.Log.Info("Found an existing loft installation")

			err = cmd.reset(kubeClient, restConfig, contextToLoad)
			if err != nil {
				return err
			}
		} else {
			cmd.Log.Info("Found an existing loft installation, if you want to redeploy loft run 'loft start --reset'")

			// get default password
			password, err := cmd.getDefaultPassword(kubeClient)
			if err != nil {
				return err
			}

			// check if local or remote installation
			_, err = kubeClient.NetworkingV1beta1().Ingresses(cmd.Namespace).Get(context.TODO(), "loft-ingress", metav1.GetOptions{})
			isLocal := kerrors.IsNotFound(err)
			if isLocal {
				// ask if we should deploy an ingress now
				const (
					NoOption  = "No"
					YesOption = "Yes, I want to deploy an ingress to let other people access loft."
				)

				answer, err := cmd.Log.Question(&survey.QuestionOptions{
					Question:     "Loft was installed without an ingress. Do you want to upgrade loft and install an ingress now?",
					DefaultValue: NoOption,
					Options: []string{
						NoOption,
						YesOption,
					},
				})
				if err != nil {
					return err
				} else if answer == YesOption {
					host, err := enterHostNameQuestion(cmd.Log)
					if err != nil {
						return err
					}

					return cmd.upgradeWithIngress(kubeClient, contextToLoad, host, password)
				}
				
				err = cmd.startPortforwarding(contextToLoad)
				if err != nil {
					return err
				}

				cmd.successLocal(password)
				return nil
			}

			// get login link
			cmd.Log.StartWait("Checking loft status...")
			host, err := cmd.getHost(kubeClient)
			cmd.Log.StopWait()
			if err != nil {
				return err
			}
			
			// check if loft is reachable
			reachable, err := isLoftReachable(host)
			if reachable == false || err != nil {
				const (
					YesOption = "Yes"
					NoOption  = "No, I want to see the DNS message again"
				)

				answer, err := cmd.Log.Question(&survey.QuestionOptions{
					Question:     "Loft seems to be not reachable at https://" + host + ". Do you want to use port-forwarding instead?",
					DefaultValue: YesOption,
					Options: []string{
						YesOption,
						NoOption,
					},
				})
				if err != nil {
					return err
				}

				if answer == YesOption {
					err := cmd.startPortforwarding(contextToLoad)
					if err != nil {
						return err
					}

					cmd.successLocal(password)
					return nil
				}
			}

			return cmd.successRemote(host, password)
		}
	}

	cmd.Log.WriteString("\n")
	cmd.Log.Info("Welcome to the loft installation.")
	cmd.Log.Info("This installer will guide you through the installation.")
	cmd.Log.Info("If you prefer installing loft via helm yourself, visit https://loft.sh/docs/getting-started/setup")
	cmd.Log.Info("Thanks for trying out loft!")

	installLocally := cmd.isLocalCluster(restConfig.Host)
	remoteHost := ""

	if installLocally == false {
		const (
			YesOption = "Yes"
			NoOption  = "No, my cluster is running locally (docker desktop, minikube, kind etc.)"
		)

		answer, err := cmd.Log.Question(&survey.QuestionOptions{
			Question:     "Seems like your cluster is running remotely (GKE, EKS, AKS, private cloud etc.). Is that correct?",
			DefaultValue: YesOption,
			Options: []string{
				YesOption,
				NoOption,
			},
		})
		if err != nil {
			return err
		}

		if answer == YesOption {
			remoteHost, err = cmd.askForHost()
			if err != nil {
				return err
			} else if remoteHost == "" {
				installLocally = true
			}
		} else {
			installLocally = true
		}
	} else {
		const (
			YesOption = "Yes"
			NoOption  = "No, I am using a remote cluster and want to access loft on a public domain"
		)

		answer, err := cmd.Log.Question(&survey.QuestionOptions{
			Question:     "Seems like your cluster is running locally (docker desktop, minikube, kind etc.). Is that correct?",
			DefaultValue: YesOption,
			Options: []string{
				YesOption,
				NoOption,
			},
		})
		if err != nil {
			return err
		}

		if answer == NoOption {
			installLocally = false

			remoteHost, err = cmd.askForHost()
			if err != nil {
				return err
			} else if remoteHost == "" {
				installLocally = true
			}
		}
	}

	userEmail, err := cmd.Log.Question(&survey.QuestionOptions{
		Question: "Enter an email address for your admin user",
		ValidationFunc: func(emailVal string) error {
			if !emailRegex.MatchString(emailVal) {
				return fmt.Errorf("%s is not a valid email address", emailVal)
			}

			parts := strings.Split(emailVal, "@")
			mx, err := net.LookupMX(parts[1])
			if err != nil || len(mx) == 0 {
				return fmt.Errorf("%s is not a valid email address", emailVal)
			}
			return nil
		},
	})
	if err != nil {
		return err
	}

	if installLocally || remoteHost == "" {
		return cmd.installLocal(kubeClient, contextToLoad, userEmail)
	}

	return cmd.installRemote(kubeClient, contextToLoad, userEmail, remoteHost)
}

func (cmd *StartCmd) reset(kubeClient kubernetes.Interface, restConfig *rest.Config, kubeContext string) error {
	cmd.Log.StartWait("Uninstalling loft...")
	defer cmd.Log.StopWait()

	deploy, err := kubeClient.AppsV1().Deployments(cmd.Namespace).Get(context.TODO(), "loft", metav1.GetOptions{})
	if err != nil {
		return err
	} else if deploy.Labels == nil || deploy.Labels["release"] == "" {
		return fmt.Errorf("loft was not installed via helm, cannot delete it then")
	}

	releaseName := deploy.Labels["release"]
	args := []string{
		"uninstall",
		releaseName,
		"--kube-context",
		kubeContext,
		"--namespace",
		cmd.Namespace,
	}
	cmd.Log.WriteString("\n")
	cmd.Log.Infof("Executing command: helm %s\n", strings.Join(args, " "))
	output, err := exec.Command("helm", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("Error during helm command: %s (%v)", string(output), err)
	}

	// wait for the loft pods to terminate
	err = wait.Poll(time.Second, time.Minute*10, func() (bool, error) {
		list, err := kubeClient.CoreV1().Pods(cmd.Namespace).List(context.TODO(), metav1.ListOptions{LabelSelector: "app=loft"})
		if err != nil {
			return false, err
		}

		return len(list.Items) == 0, nil
	})
	if err != nil {
		return err
	}

	// we also cleanup the validating webhook configuration and apiservice
	err = kubeClient.AdmissionregistrationV1().ValidatingWebhookConfigurations().Delete(context.TODO(), "loft", metav1.DeleteOptions{})
	if err != nil && kerrors.IsNotFound(err) == false {
		return err
	}

	apiRegistrationClient, err := clientset.NewForConfig(restConfig)
	if err != nil {
		return err
	}

	err = apiRegistrationClient.ApiregistrationV1().APIServices().Delete(context.TODO(), "v1.management.loft.sh", metav1.DeleteOptions{})
	if err != nil && kerrors.IsNotFound(err) == false {
		return err
	}

	loftClient, err := loftclientset.NewForConfig(restConfig)
	if err != nil {
		return err
	}

	err = loftClient.StorageV1().Users().Delete(context.TODO(), "admin", metav1.DeleteOptions{})
	if err != nil && kerrors.IsNotFound(err) == false {
		return err
	}

	cmd.Log.StopWait()
	cmd.Log.Done("Successfully uninstalled loft")
	return nil
}

func (cmd *StartCmd) isLoftAlreadyInstalled(kubeClient kubernetes.Interface) (bool, error) {
	_, err := kubeClient.AppsV1().Deployments(cmd.Namespace).Get(context.TODO(), "loft", metav1.GetOptions{})
	if err != nil {
		if kerrors.IsNotFound(err) == true {
			return false, nil
		}

		return false, fmt.Errorf("error accessing kubernetes cluster: %v", err)
	}

	return true, nil
}

func (cmd *StartCmd) askForHost() (string, error) {
	ingressAccess := "via ingress (you will need to configure DNS)"
	answer, err := cmd.Log.Question(&survey.QuestionOptions{
		Question: "How do you want to access loft?",
		Options: []string{
			"via port-forwarding (no other configuration needed)",
			ingressAccess,
		},
	})
	if err != nil {
		return "", err
	}

	if answer == ingressAccess {
		return enterHostNameQuestion(cmd.Log)
	}

	return "", nil
}

func enterHostNameQuestion(log log.Logger) (string, error) {
	return log.Question(&survey.QuestionOptions{
		Question: "Enter a hostname for your loft instance (e.g. loft.my-domain.tld): \n",
		ValidationFunc: func(answer string) error {
			u, err := url.Parse("https://" + answer)
			if err != nil || u.Path != "" || u.Port() != "" || len(strings.Split(answer, ".")) < 2 {
				return fmt.Errorf("Please enter a valid hostname without protocol (https://), without path and without port, e.g. loft.my-domain.tld")
			}
			return nil
		},
	})
}

var privateIPBlocks []*net.IPNet

func init() {
	for _, cidr := range []string{
		"127.0.0.0/8",    // IPv4 loopback
		"10.0.0.0/8",     // RFC1918
		"172.16.0.0/12",  // RFC1918
		"192.168.0.0/16", // RFC1918
		"::1/128",        // IPv6 loopback
		"fe80::/10",      // IPv6 link-local
		"fc00::/7",       // IPv6 unique local addr
	} {
		_, block, _ := net.ParseCIDR(cidr)
		privateIPBlocks = append(privateIPBlocks, block)
	}
}

// IsPrivateIP checks if a given ip is private
func IsPrivateIP(ip net.IP) bool {
	for _, block := range privateIPBlocks {
		if block.Contains(ip) {
			return true
		}
	}

	return false
}

func (cmd *StartCmd) isLocalCluster(host string) bool {
	url, err := url.Parse(host)
	if err != nil {
		cmd.Log.Warnf("Couldn't parse kube context host url: %v", err)
		return false
	}

	hostname := url.Hostname()
	ip := net.ParseIP(hostname)
	if ip != nil {
		if IsPrivateIP(ip) {
			return true
		}
	}

	if hostname == "localhost" || strings.HasSuffix(hostname, ".internal") || strings.HasSuffix(hostname, ".localhost") {
		return true
	}

	return false
}

func (cmd *StartCmd) installIngressController(kubeClient kubernetes.Interface, kubeContext string) error {
	// first create an ingress controller
	const (
		YesOption = "Yes"
		NoOption  = "No, I already have an ingress controller installed"
	)

	answer, err := cmd.Log.Question(&survey.QuestionOptions{
		Question:     "Ingress controller required. Should the nginx-ingress controller be installed?",
		DefaultValue: YesOption,
		Options: []string{
			YesOption,
			NoOption,
		},
	})
	if err != nil {
		return err
	}
	if answer == YesOption {
		args := []string{
			"install",
			"ingress-nginx",
			"ingress-nginx",
			"--repository-config=''",
			"--repo",
			"https://kubernetes.github.io/ingress-nginx",
			"--kube-context",
			kubeContext,
			"--namespace",
			"ingress-nginx",
			"--create-namespace",
			"--set-string",
			"controller.config.hsts=false",
			"--wait",
		}
		cmd.Log.WriteString("\n")
		cmd.Log.Infof("Executing command: helm %s\n", strings.Join(args, " "))
		cmd.Log.StartWait("Waiting for ingress controller deployment, this can take several minutes...")
		output, err := exec.Command("helm", args...).CombinedOutput()
		cmd.Log.StopWait()
		if err != nil {
			return fmt.Errorf("Error during helm command: %s (%v)", string(output), err)
		}

		list, err := kubeClient.CoreV1().Secrets("ingress-nginx").List(context.TODO(), metav1.ListOptions{
			LabelSelector: "name=ingress-nginx,owner=helm,status=deployed",
		})
		if err != nil {
			return err
		}

		if len(list.Items) == 1 {
			secret := list.Items[0]
			originalSecret := secret.DeepCopy()
			secret.Labels["loft.sh/app"] = "true"
			if secret.Annotations == nil {
				secret.Annotations = map[string]string{}
			}

			secret.Annotations["loft.sh/url"] = "https://kubernetes.github.io/ingress-nginx"
			originalJSON, err := json.Marshal(originalSecret)
			if err != nil {
				return err
			}
			modifiedJSON, err := json.Marshal(secret)
			if err != nil {
				return err
			}
			data, err := jsonpatch.CreateMergePatch(originalJSON, modifiedJSON)
			if err != nil {
				return err
			}
			_, err = kubeClient.CoreV1().Secrets(secret.Namespace).Patch(context.TODO(), secret.Name, types.MergePatchType, data, metav1.PatchOptions{})
			if err != nil {
				return err
			}
		}

		cmd.Log.Done("Successfully installed ingress-nginx to your kubernetes cluster!")
	}
	
	return nil
}

func (cmd *StartCmd) installRemote(kubeClient kubernetes.Interface, kubeContext, email, host string) error {
	err := cmd.installIngressController(kubeClient, kubeContext)
	if err != nil {
		return errors.Wrap(err, "install ingress controller")
	}

	password, err := cmd.getDefaultPassword(kubeClient)
	if err != nil {
		return err
	}

	// now we install loft
	args := []string{
		"install",
		"loft",
		"loft",
		"--repository-config=''",
		"--repo",
		"https://charts.devspace.sh/",
		"--kube-context",
		kubeContext,
		"--namespace",
		cmd.Namespace,
		"--set",
		"certIssuer.create=false",
		"--set",
		"ingress.host=" + host,
		"--set",
		"cluster.connect.local=true",
		"--set",
		"admin.password=" + password,
		"--set",
		"admin.email=" + email,
		"--wait",
	}
	if cmd.Version != "" {
		args = append(args, "--version", cmd.Version)
	}

	cmd.Log.WriteString("\n")
	cmd.Log.Infof("Executing command: helm %s\n", strings.Join(args, " "))
	cmd.Log.StartWait("Waiting for loft deployment, this can take several minutes...")
	output, err := exec.Command("helm", args...).CombinedOutput()
	cmd.Log.StopWait()
	if err != nil {
		return fmt.Errorf("error during helm command: %s (%v)", string(output), err)
	}

	cmd.Log.Done("Successfully deployed loft to your kubernetes cluster!")
	cmd.Log.WriteString("\n")
	cmd.Log.StartWait("Waiting until loft pod has been started...")
	defer cmd.Log.StopWait()
	err = cmd.waitForReadyLoftPod(kubeClient)
	if err != nil {
		return err
	}
	cmd.Log.StopWait()
	cmd.Log.Done("Loft pod has successfully started")

	return cmd.successRemote(host, password)
}

func (cmd *StartCmd) upgradeWithIngress(kubeClient kubernetes.Interface, kubeContext, host, password string) error {
	err := cmd.installIngressController(kubeClient, kubeContext)
	if err != nil {
		return errors.Wrap(err, "install ingress controller")
	}
	
	// now we install loft
	args := []string{
		"upgrade",
		"loft",
		"loft",
		"--repository-config=''",
		"--repo",
		"https://charts.devspace.sh/",
		"--kube-context",
		kubeContext,
		"--namespace",
		cmd.Namespace,
		"--reuse-values",
		"--set",
		"ingress.enabled=true",
		"--set",
		"ingress.host=" + host,
		"--wait",
	}
	if cmd.Version != "" {
		args = append(args, "--version", cmd.Version)
	}

	cmd.Log.WriteString("\n")
	cmd.Log.Infof("Executing command: helm %s\n", strings.Join(args, " "))
	cmd.Log.StartWait("Waiting for loft, this can take several minutes...")
	output, err := exec.Command("helm", args...).CombinedOutput()
	cmd.Log.StopWait()
	if err != nil {
		return fmt.Errorf("error during helm command: %s (%v)", string(output), err)
	}

	cmd.Log.Done("Successfully upgraded loft to use an ingress!")
	cmd.Log.WriteString("\n")

	return cmd.successRemote(host, password)
}

func (cmd *StartCmd) installLocal(kubeClient kubernetes.Interface, kubeContext, email string) error {
	cmd.Log.WriteString("\n")
	cmd.Log.Info("This will install loft without an externally reachable URL and instead use port-forwarding to connect to loft")
	cmd.Log.WriteString("\n")

	password, err := cmd.getDefaultPassword(kubeClient)
	if err != nil {
		return err
	}

	args := []string{
		"install",
		"loft",
		"loft",
		"--repository-config=''",
		"--repo",
		"https://charts.devspace.sh/",
		"--kube-context",
		kubeContext,
		"--namespace",
		cmd.Namespace,
		"--set",
		"certIssuer.create=false",
		"--set",
		"ingress.enabled=false",
		"--set",
		"cluster.connect.local=true",
		"--set",
		"admin.password=" + password,
		"--set",
		"admin.email=" + email,
		"--wait",
	}
	if cmd.Version != "" {
		args = append(args, "--version", cmd.Version)
	}

	cmd.Log.Infof("Executing command: helm %s\n", strings.Join(args, " "))
	cmd.Log.StartWait("Waiting for loft deployment, this can take several minutes...")
	output, err := exec.Command("helm", args...).CombinedOutput()
	cmd.Log.StopWait()
	if err != nil {
		return fmt.Errorf("Error during helm command: %s (%v)", string(output), err)
	}

	cmd.Log.WriteString("\n")
	cmd.Log.Done("Successfully deployed loft to your kubernetes cluster!")
	cmd.Log.StartWait("Waiting until loft pod has been started...")
	defer cmd.Log.StopWait()
	err = cmd.waitForReadyLoftPod(kubeClient)
	if err != nil {
		return err
	}
	cmd.Log.StopWait()

	err = cmd.startPortforwarding(kubeContext)
	if err != nil {
		return err
	}

	cmd.successLocal(password)
	return nil
}

type version struct {
	Version string `json:"version"`
}

func isLoftReachable(host string) (bool, error) {
	// wait until loft is reachable at the given url
	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true,
			},
		},
	}
	url := "https://" + host + "/version"
	resp, err := client.Get(url)
	if err == nil && resp.StatusCode == http.StatusOK {
		out, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return false, nil
		}
		
		v := &version{}
		err = json.Unmarshal(out, v)
		if err != nil {
			return false, fmt.Errorf("error decoding response from %s: %v. Try running 'loft start --reset'", url, err)
		} else if v.Version == "" {
			return false, fmt.Errorf("unexpected response from %s: %s. Try running 'loft start --reset'", url, string(out))
		}
		
		return true, nil
	}
	
	return false, nil
}

func (cmd *StartCmd) successRemote(host string, password string) error {
	ready, err := isLoftReachable(host)
	if err != nil {
		return err
	} else if ready {
		cmd.successMessageRemote(host, password)
		return nil
	}

	cmd.Log.WriteString(`

###################################     DNS CONFIGURATION REQUIRED     ##################################

Create a DNS A-record for ` + host + ` with the EXTERNAL-IP of your nginx-ingress controller.
To find this EXTERNAL-IP, run the following command and look at the output:

> kubectl get services -n ingress-nginx
                                                     |---------------|
NAME                       TYPE           CLUSTER-IP | EXTERNAL-IP   |  PORT(S)                      AGE
ingress-nginx-controller   LoadBalancer   10.0.0.244 | XX.XXX.XXX.XX |  80:30984/TCP,443:31758/TCP   19m
                                                     |^^^^^^^^^^^^^^^|

EXTERNAL-IP may be 'pending' for a while until your cloud provider has created a new load balancer.

#########################################################################################################

The command will wait until loft is reachable under the host. You can also abort and use port-forwarding instead
by running 'loft start' again.

`)

	cmd.Log.StartWait("Waiting for you to configure DNS, so loft can be reached on https://" + host)
	err = wait.PollImmediate(time.Second*5, time.Hour*24, func() (bool, error) {
		return isLoftReachable(host)
	})
	cmd.Log.StopWait()
	if err != nil {
		return err
	}

	cmd.Log.Done("loft is reachable at https://" + host)
	cmd.successMessageRemote(host, password)
	return nil
}

func (cmd *StartCmd) successMessageRemote(host, password string) {
	url := "https://" + host
	cmd.Log.WriteString(`


##########################   LOGIN   ############################

Username: ` + ansi.Color("admin", "green+b") + `
Password: ` + ansi.Color(password, "green+b") + `

Login via UI:  ` + ansi.Color(url, "green+b") + `
Login via CLI: ` + ansi.Color(`loft login --insecure `+url, "green+b") + `

!!! You must accept the untrusted certificate in your browser !!!

Follow this guide to add a valid certificate: https://loft.sh/docs/administration/ssl

#################################################################

Loft was successfully installed and can now be reached at: ` + url + `

Thanks for using loft!
`)
}

func (cmd *StartCmd) successLocal(password string) {
	url := "https://localhost:" + cmd.LocalPort
	cmd.Log.WriteString(`

##########################   LOGIN   ############################

Username: ` + ansi.Color("admin", "green+b") + `
Password: ` + ansi.Color(password, "green+b") + `

Login via UI:  ` + ansi.Color(url, "green+b") + `
Login via CLI: ` + ansi.Color(`loft login --insecure `+url, "green+b") + `

!!! You must accept the untrusted certificate in your browser !!!

#################################################################

Loft was successfully installed and port-forwarding has been started.
If you stop this command, run 'loft start' again to restart port-forwarding.

Thanks for using loft!
`)

	blockChan := make(chan bool)
	<-blockChan
}

func (cmd *StartCmd) getDefaultPassword(kubeClient kubernetes.Interface) (string, error) {
	if cmd.Password != "" {
		return cmd.Password, nil
	}

	loftNamespace, err := kubeClient.CoreV1().Namespaces().Get(context.TODO(), cmd.Namespace, metav1.GetOptions{})
	if err != nil {
		if kerrors.IsNotFound(err) {
			loftNamespace, err := kubeClient.CoreV1().Namespaces().Create(context.TODO(), &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: cmd.Namespace,
				},
			}, metav1.CreateOptions{})
			if err != nil {
				return "", err
			}

			return string(loftNamespace.UID), nil
		}

		return "", err
	}

	return string(loftNamespace.UID), nil
}

func (cmd *StartCmd) startPortforwarding(kubeContext string) error {
	cmd.Log.WriteString("\n")
	cmd.Log.Info("Loft will now start port-forwarding to the loft pod")
	args := []string{
		"port-forward",
		"deploy/loft",
		"--context",
		kubeContext,
		"--namespace",
		cmd.Namespace,
		cmd.LocalPort + ":443",
	}
	cmd.Log.Infof("Starting command: kubectl %s", strings.Join(args, " "))

	c := exec.Command("kubectl", args...)
	// c.Stderr = os.Stderr
	// c.Stdout = os.Stdout

	err := c.Start()
	if err != nil {
		return fmt.Errorf("error starting kubectl command: %v", err)
	}
	go func() {
		err := c.Wait()
		if err != nil {
			cmd.Log.Fatal("Port-Forwarding has unexpectedly ended. Please restart the command via 'loft start'")
		}
	}()

	// wait until loft is reachable at the given url
	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true,
			},
		},
	}

	cmd.Log.Infof("Waiting until loft is reachable at https://localhost:%s", cmd.LocalPort)
	return wait.PollImmediate(time.Second, time.Minute*10, func() (bool, error) {
		resp, err := client.Get("https://localhost:" + cmd.LocalPort + "/version")
		if err != nil {
			return false, nil
		}

		return resp.StatusCode == http.StatusOK, nil
	})
}

func (cmd *StartCmd) getHost(kubeClient kubernetes.Interface) (string, error) {
	ingress, err := kubeClient.NetworkingV1beta1().Ingresses(cmd.Namespace).Get(context.TODO(), "loft-ingress", metav1.GetOptions{})
	if err != nil {
		return "", err
	}

	// find host
	for _, rule := range ingress.Spec.Rules {
		return rule.Host, nil
	}
	return "", fmt.Errorf("couldn't find any host in loft ingress '%s/loft-ingress', please make sure you have not changed any deployed resources")
}

func (cmd *StartCmd) waitForReadyLoftPod(kubeClient kubernetes.Interface) error {
	// wait until we have a running loft pod
	err := wait.PollImmediate(time.Second*3, time.Minute*10, func() (bool, error) {
		pods, err := kubeClient.CoreV1().Pods(cmd.Namespace).List(context.TODO(), metav1.ListOptions{
			LabelSelector: "app=loft",
		})
		if err != nil {
			cmd.Log.Warnf("Error trying to retrieve loft pod: %v", err)
			return false, nil
		} else if len(pods.Items) == 0 {
			return false, nil
		}

		loftPod := &pods.Items[0]
		for _, containerStatus := range loftPod.Status.ContainerStatuses {
			if containerStatus.State.Running != nil && containerStatus.Ready {
				continue
			} else if containerStatus.State.Terminated != nil && containerStatus.State.Terminated.ExitCode != 0 {
				cmd.Log.Warnf("There seems to be an issue with loft starting up: %s (%s). Please reach out to our support at https://loft.sh/", containerStatus.State.Terminated.Message, containerStatus.State.Terminated.Reason)
				continue
			}

			return false, nil
		}

		return true, nil
	})
	return err
}
