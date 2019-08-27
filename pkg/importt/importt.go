package importt

import (
	"fmt"

	"github.com/logicmonitor/k8s-release-manager/pkg/client"
	"github.com/logicmonitor/k8s-release-manager/pkg/config"
	"github.com/logicmonitor/k8s-release-manager/pkg/constants"
	"github.com/logicmonitor/k8s-release-manager/pkg/lmhelm"
	"github.com/logicmonitor/k8s-release-manager/pkg/release"
	"github.com/logicmonitor/k8s-release-manager/pkg/state"
	log "github.com/sirupsen/logrus"
	rls "k8s.io/helm/pkg/proto/hapi/release"
)

// Import remotely stored releases
type Import struct {
	Config     *config.Config
	HelmClient *lmhelm.Client
	State      *state.State
}

// New instantiates and returns a Deleter and an error if any.
func New(rlsmgrconfig *config.Config, state *state.State) (*Import, error) {
	helmClient := &lmhelm.Client{}

	kubernetesClient, kubernetesConfig, err := client.KubernetesClient(rlsmgrconfig.ClusterConfig)
	if err != nil {
		return nil, err
	}

	err = helmClient.Init(rlsmgrconfig.Helm, kubernetesClient, kubernetesConfig)
	if err != nil {
		return nil, err
	}
	return &Import{
		Config:     rlsmgrconfig,
		HelmClient: helmClient,
		State:      state,
	}, nil
}

// Run the Import
func (t *Import) Run() error {
	releases, err := t.State.Releases.StoredReleases()
	if err != nil {
		log.Fatalf("Error retrieving stored releases: %v", err)
	}

	releases, err = t.processReleases(releases)
	if err != nil {
		return err
	}

	err = t.State.Read()
	if err != nil {
		return err
	}

	err = t.sanityCheck()
	if err != nil {
		return err
	}
	return t.deployReleases(releases)
}

func (t *Import) processReleases(releases []*rls.Release) ([]*rls.Release, error) {
	releases = t.filterReleasesByNamespace(releases)
	return t.updateValues(releases)
}

func (t *Import) filterReleasesByNamespace(releases []*rls.Release) []*rls.Release {
	if t.Config.Import.Namespace != "" {
		return t.includeReleasesByNamespace(releases)
	}
	if len(t.Config.Import.ExcludeNamespaces) > 0 {
		return t.excludeReleasesByNamespace(releases)
	}
	return releases
}

func (t *Import) includeReleasesByNamespace(releases []*rls.Release) []*rls.Release {
	var deploy []*rls.Release
	for _, r := range releases {
		if r.Namespace == t.Config.Import.Namespace {
			deploy = append(deploy, r)
		}
	}
	return deploy
}

func (t *Import) excludeReleasesByNamespace(releases []*rls.Release) []*rls.Release {
	var deploy []*rls.Release
	for _, r := range releases {
		bMatch := false
		for _, namespace := range t.Config.Import.ExcludeNamespaces {
			if r.Namespace == namespace {
				bMatch = true
				break
			}
		}
		if bMatch {
			continue
		}
		deploy = append(deploy, r)
	}
	return deploy
}

func (t *Import) updateValues(releases []*rls.Release) ([]*rls.Release, error) {
	log.Debugf("Updating release values\n")
	var err error
	for _, r := range releases {
		for k, v := range t.Config.Import.Values {
			r, err = release.UpdateValue(r, k, v)
			if err != nil {
				return nil, err
			}
		}
	}
	return releases, nil
}

func (t *Import) deployReleases(releases []*rls.Release) error {
	var err error
	var sem = make(chan int, constants.ImportMaxThreads)
	for _, r := range releases {
		fmt.Printf("Deploying release %s to namespace %s\n", r.GetName(), r.GetNamespace())

		// update the target namespace if option specified
		if t.Config.Import.Target != "" {
			r.Namespace = t.Config.Import.Target
		}

		r, err = t.updateManagerRelease(r)
		if err != nil {
			log.Errorf("Unable to update the output path for the new release manager chart. Skipping.")
			continue
		}

		if t.Config.DryRun {
			fmt.Printf("%s\n", release.ToString(r, t.Config.VerboseMode))
			continue
		}

		sem <- 1
		go func(r *rls.Release) error {
			defer func() { <-sem }()
			return t.deployRelease(r)
		}(r)
	}

	// wait for installs to finish
	for i := 0; i < cap(sem); i++ {
		sem <- 1
	}
	return nil
}

func (t *Import) deployRelease(r *rls.Release) error {
	err := t.HelmClient.Install(r)
	if err != nil {
		if lmhelm.ErrorReleaseExists(err) {
			fmt.Printf("Skipping release: %s already exists\n", r.GetName())
		} else {
			fmt.Printf("Error deploying release %s: %v\n", r.GetName(), err)
			return err
		}
	} else {
		fmt.Printf("Successfully deployed release %s\n", r.GetName())
	}
	return nil
}

// if this is the release manager release, update the backend path, else return unmodified
func (t *Import) updateManagerRelease(r *rls.Release) (*rls.Release, error) {
	if t.Config.Import.NewStoragePath == "" || t.State.Info == nil || r.GetName() != t.State.Info.ReleaseName {
		return r, nil
	}
	return t.updateManagerStoragePath(r, t.Config.Import.NewStoragePath)
}

func (t *Import) updateManagerStoragePath(r *rls.Release, path string) (*rls.Release, error) {
	return release.UpdateValue(r, constants.ValueStoragePath, path)
}

// if a state file exists but --new-path wasn't specified, this is probably bad.
// the newly installed release manager chart will get installed with the same
// remote path as the manager previously configured to write to the same remote path.
// having two release managers from different clusters writing to the same remote path
// is going to cause all sorts of issues, including, but not limited to,
// overwriting each other's release state.
// do sanity checks here.
func (t *Import) sanityCheck() error {
	switch true {
	case t.State.Info != nil && t.Config.Import.NewStoragePath == "":
		return t.resolveStateConflict()
	case t.State.Info == nil && t.Config.Import.NewStoragePath != "":
		log.Warnf("--path specified but no remote state found.")
		return nil
	case t.State.Info == nil && t.Config.Import.NewStoragePath == "":
		return nil
	case t.State.Info != nil && t.Config.Import.NewStoragePath != "":
		return nil
	default:
		return fmt.Errorf("Unknown error performing state sanity checks. Failing")
	}
}

func (t *Import) resolveStateConflict() error {
	warn := "This can lead to unexpected results and is probably a mistake. If you really wish to continue, use --force"
	msg := fmt.Sprintf(
		"Existing state %s exists in path %s but --new-path wasn't specified.",
		t.State.Name(),
		t.Config.Backend.StoragePath,
	)

	// in case the user REALLY wants to proceed anyway
	if t.Config.Import.Force {
		log.Warnf("%s\n--force specified. Proceeding...", msg)
		return nil
	}

	if t.Config.DryRun {
		fmt.Printf("%s\n%s\n", msg, warn)
		return nil
	}
	return fmt.Errorf("%s\n%s", msg, warn)
}
