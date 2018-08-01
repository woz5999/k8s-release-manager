package transfer

import (
	"fmt"
	"sync"

	"github.com/logicmonitor/k8s-release-manager/pkg/client"
	"github.com/logicmonitor/k8s-release-manager/pkg/config"
	"github.com/logicmonitor/k8s-release-manager/pkg/constants"
	"github.com/logicmonitor/k8s-release-manager/pkg/lmhelm"
	"github.com/logicmonitor/k8s-release-manager/pkg/release"
	"github.com/logicmonitor/k8s-release-manager/pkg/state"
	log "github.com/sirupsen/logrus"
	rls "k8s.io/helm/pkg/proto/hapi/release"
)

// Transfer deploys remotely stored releases
type Transfer struct {
	Config     *config.Config
	HelmClient *lmhelm.Client
	State      *state.State
}

// New instantiates and returns a Deleter and an error if any.
func New(rlsmgrconfig *config.Config, state *state.State) (*Transfer, error) {
	helmClient := &lmhelm.Client{}

	kubernetesClient, kubernetesConfig, err := client.KubernetesClient(rlsmgrconfig.ClusterConfig)
	if err != nil {
		return nil, err
	}

	err = helmClient.Init(rlsmgrconfig.Helm, kubernetesClient, kubernetesConfig)
	if err != nil {
		return nil, err
	}
	return &Transfer{
		Config:     rlsmgrconfig,
		HelmClient: helmClient,
		State:      state,
	}, nil
}

// Run the Transfer.
func (t *Transfer) Run() error {
	releases, err := t.State.Releases.StoredReleases()
	if err != nil {
		log.Fatalf("Error retrieving stored releases: %v", err)
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

func (t *Transfer) deployReleases(releases []*rls.Release) error {
	var err error
	var wg sync.WaitGroup
	for _, r := range releases {
		fmt.Printf("Deploying release: %s\n", r.GetName())

		r, err = t.updateManagerRelease(r)
		if err != nil {
			log.Errorf("Unable to update the output path for the new release manager chart. Skipping.")
			continue
		}

		if t.Config.DryRun {
			fmt.Printf("%s\n", release.ToString(r, t.Config.VerboseMode))
			continue
		}

		wg.Add(1)
		go func(r *rls.Release) {
			defer wg.Done()
			t.deployRelease(r)
		}(r)
	}
	wg.Wait()
	return nil
}

func (t *Transfer) deployRelease(r *rls.Release) {
	err := t.HelmClient.Install(r)
	if err != nil {
		log.Errorf("Error deploying release %s: %v", r.GetName(), err)
	}
}

// if this is the release manager release, update the backend path, else return unmodified
func (t *Transfer) updateManagerRelease(r *rls.Release) (*rls.Release, error) {
	if t.Config.Transfer.NewStoragePath == "" || t.State.Info == nil || r.GetName() != t.State.Info.ReleaseName {
		return r, nil
	}
	return t.updateManagerStoragePath(r, t.Config.Transfer.NewStoragePath)
}

func (t *Transfer) updateManagerStoragePath(r *rls.Release, path string) (*rls.Release, error) {
	return release.UpdateValue(r, constants.ValueStoragePath, path)
}

// if a state file exists but --new-path wasn't specified, this is probably bad.
// the newly installed release manager chart will get installed with the same
// remote path as the manager previously configured to write to the same remote path.
// having two release managers from different clusters writing to the same remote path
// is going to cause all sorts of issues, including, but not limited to,
// overwriting each other's release state.
// do sanity checks here.
func (t *Transfer) sanityCheck() error {
	switch true {
	case t.State != nil && t.Config.Transfer.NewStoragePath == "":
		return t.resolveStateConflict()
	case t.State == nil && t.Config.Transfer.NewStoragePath != "":
		log.Warnf("--path specified but no remote state found.")
		return nil
	case t.State == nil && t.Config.Transfer.NewStoragePath == "":
		return nil
	case t.State != nil && t.Config.Transfer.NewStoragePath != "":
		return nil
	default:
		return fmt.Errorf("Unknown error performing state sanity checks. Failing")
	}
}

func (t *Transfer) resolveStateConflict() error {
	warn := "This can lead to unexpected results and is probably a mistake. If you really wish to continue, use --force"
	msg := fmt.Sprintf(
		"Existing state exists at %s but --new-path wasn't specified.",
		t.State.Path(),
	)

	// in case the user REALLY wants to proceed anyway
	if t.Config.Transfer.Force {
		log.Warnf("%s\n--force specified. Proceeding...", msg)
		return nil
	}

	if t.Config.DryRun {
		fmt.Printf("%s\n%s\n", msg, warn)
		return nil
	}
	return fmt.Errorf("%s\n%s", msg, warn)
}
