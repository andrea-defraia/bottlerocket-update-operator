package agent

import (
	"context"
	"fmt"
	"os"

	"github.com/amazonlinux/thar/dogswatch/pkg/intent"
	"github.com/amazonlinux/thar/dogswatch/pkg/k8sutil"
	"github.com/amazonlinux/thar/dogswatch/pkg/logging"
	"github.com/amazonlinux/thar/dogswatch/pkg/marker"
	"github.com/amazonlinux/thar/dogswatch/pkg/nodestream"
	"github.com/amazonlinux/thar/dogswatch/pkg/platform"
	"github.com/amazonlinux/thar/dogswatch/pkg/platform/updog"
	"github.com/amazonlinux/thar/dogswatch/pkg/workgroup"
	"github.com/pkg/errors"
	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
)

var (
	errInvalidProgress = errors.New("intended to make invalid progress")
)

type Agent struct {
	log      logging.Logger
	kube     kubernetes.Interface
	platform platform.Platform

	state    *nodeState
	progress progression
}

func New(log logging.Logger, kube kubernetes.Interface, plat platform.Platform) *Agent {
	return &Agent{
		log:      log,
		kube:     kube,
		platform: plat,
		state:    initialState(),
	}
}

func (a *Agent) check() error {
	switch {
	case a.kube == nil:
		return errors.New("kubernetes client is nil")
	case a.platform == nil:
		return errors.New("supporting platform is nil")
	}
	return nil
}

func (a *Agent) Run(ctx context.Context) error {
	if err := a.check(); err != nil {
		return errors.WithMessage(err, "misconfigured")
	}
	a.log.Debug("starting")
	defer a.log.Debug("finished")
	group := workgroup.WithContext(ctx)

	ns := nodestream.New(a.log.WithField("worker", "informer"), a.kube, nodestream.Config{
		NodeName: "minikube",
	}, a.handler())

	group.Work(ns.Run)

	err := a.nodePreflight()
	if err != nil {
		return err
	}

	select {
	case <-ctx.Done():
		a.log.Info("waiting on workers to finish")
		return group.Wait()
	}
}

func (a *Agent) handler() nodestream.Handler {
	return &nodestream.HandlerFuncs{
		OnAddFunc: a.handleEvent,
		// we don't mind the diff between old and new, so handle the new
		// resource.
		OnUpdateFunc: func(_, n *v1.Node) {
			a.handleEvent(n)
		},
		OnDeleteFunc: func(_ *v1.Node) {
			panic("we were deleted, panic. everyone panic. 😱")
		},
	}
}

func (a *Agent) handleEvent(node *v1.Node) {
	in := intent.Given(node)
	if activeIntent(in) {
		a.log.Debug("active intent received")
		if err := a.realize(in); err != nil {
			a.log.WithError(err).Error("could not handle intent")
		}
		return
	}
	a.log.Debug("inactive intent received")
}

func activeIntent(i *intent.Intent) bool {
	return i.WantProgress() && // the intent will make progress
		!i.Errored() && // its not currently in an errored state
		!i.Waiting() // its not waiting on a command, ie *this* is the intentional command
}

func (a *Agent) realize(in *intent.Intent) error {
	a.log.WithField("intent", fmt.Sprintf("%#v", in)).Debug("realizing intent")
	updateIntent := func(uin *intent.Intent) error {
		uerr := k8sutil.PostMetadata(a.kube.CoreV1().Nodes(), uin.NodeName, uin)
		if uerr != nil {
			a.log.WithError(uerr).Error("could not update markers")
			uerr = errors.WithMessage(uerr, "could not update markers")
			return uerr
		}
		return nil
	}

	var err error

	// TODO: implement and wire up updog consistent update invocations
	if a.progress.GetTarget() == nil {
		a.progress.SetTarget(&updog.NoopUpdate{})
	}

	// TODO: Sanity check progression before proceeding

	// ACK the wanted action.
	in.Active = in.Wanted
	in.State = marker.NodeStateBusy
	if err = updateIntent(in); err != nil {
		return err
	}

	// TODO: Propagate status from realization and periodically
	switch in.Wanted {
	case marker.NodeActionReset:
		a.progress.Reset()

	case marker.NodeActionPrepareUpdate:
		var ups platform.Available
		ups, err = a.platform.ListAvailable()
		if err != nil {
			break
		}
		if len(ups.Updates()) == 0 {
			err = errInvalidProgress
			break
		}
		a.progress.SetTarget(ups.Updates()[0])
		a.log.Debug("preparing update")
		err = a.platform.Prepare(a.progress.GetTarget())

	case marker.NodeActionPerformUpdate:
		if !a.progress.Valid() {
			err = errInvalidProgress
			break
		}
		a.log.Debug("updating")
		err = a.platform.Update(a.progress.GetTarget())

	case marker.NodeActionUnknown, marker.NodeActionStablize:
		if !a.progress.Valid() {
			err = errInvalidProgress
			break
		}
		a.log.Debug("sitrep")
		_, err = a.platform.Status()

	case marker.NodeActionRebootUpdate:
		if !a.progress.Valid() {
			err = errInvalidProgress
			break
		}
		a.log.Debug("rebooting")
		a.log.Info("Rebooting Node to complete update")
		// TODO: ensure Node is setup to be validated on boot (ie: kubelet will
		// run agent again before we let other Pods get scheduled)
		err = a.platform.BootUpdate(a.progress.GetTarget(), true)

		// Shortcircuit to terminate.

		// TODO: actually handle shutdown.
		{
			// die("goodbye");
			p, _ := os.FindProcess(os.Getpid())
			go p.Kill()

		}
		return nil
	}

	if err != nil {
		in.State = marker.NodeStateError
	} else {
		in.State = marker.NodeStateReady
	}

	updateIntent(in)

	return err
}

func (a *Agent) nodePreflight() error {
	// TODO: sanity check node and reset appropriate Resource state
	// TODO: inform controller for taint removal
	return nil
}
