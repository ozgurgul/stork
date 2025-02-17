package controllers

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/hashicorp/go-multierror"
	snap_v1 "github.com/kubernetes-incubator/external-storage/snapshot/pkg/apis/crd/v1"
	"github.com/libopenstorage/stork/drivers/volume"
	stork_api "github.com/libopenstorage/stork/pkg/apis/stork/v1alpha1"
	"github.com/libopenstorage/stork/pkg/controllers"
	"github.com/libopenstorage/stork/pkg/k8sutils"
	"github.com/libopenstorage/stork/pkg/log"
	"github.com/libopenstorage/stork/pkg/version"
	"github.com/portworx/sched-ops/k8s/apiextensions"
	"github.com/portworx/sched-ops/k8s/core"
	k8sextops "github.com/portworx/sched-ops/k8s/externalstorage"
	storkops "github.com/portworx/sched-ops/k8s/stork"
	"github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	apiextensionsv1beta1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1beta1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/tools/record"
	runtimeclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	annotationPrefix   = "stork.libopenstorage.org/"
	storkSchedulerName = "stork"
	// RestoreAnnotation for pvc which has in-place restore in progress
	RestoreAnnotation            = annotationPrefix + "restore-in-progress"
	validateSnapshotTimeout      = 1 * time.Minute
	validateSnapshotRetryTimeout = 5 * time.Second
)

// NewSnapshotRestoreController creates a new instance of SnapshotRestoreController.
func NewSnapshotRestoreController(mgr manager.Manager, d volume.Driver, r record.EventRecorder) *SnapshotRestoreController {
	return &SnapshotRestoreController{
		client:    mgr.GetClient(),
		volDriver: d,
		recorder:  r,
	}
}

// SnapshotRestoreController controller to watch over In-Place snap restore CRD's
type SnapshotRestoreController struct {
	client runtimeclient.Client

	volDriver volume.Driver
	recorder  record.EventRecorder
}

// Init initialize the cluster pair controller
func (c *SnapshotRestoreController) Init(mgr manager.Manager) error {
	err := c.createCRD()
	if err != nil {
		return err
	}

	return controllers.RegisterTo(mgr, "snapshot-restore-controller", c, &stork_api.VolumeSnapshotRestore{})
}

// Reconcile manages SnapShot resources.
func (c *SnapshotRestoreController) Reconcile(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {
	logrus.Tracef("Reconciling VolumeSnapshotRestore %s/%s", request.Namespace, request.Name)

	// Fetch the ApplicationBackup instance
	restore := &stork_api.VolumeSnapshotRestore{}
	err := c.client.Get(context.TODO(), request.NamespacedName, restore)
	if err != nil {
		if errors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return reconcile.Result{RequeueAfter: controllers.DefaultRequeueError}, err
	}

	if !controllers.ContainsFinalizer(restore, controllers.FinalizerCleanup) {
		controllers.SetFinalizer(restore, controllers.FinalizerCleanup)
		return reconcile.Result{Requeue: true}, c.client.Update(context.TODO(), restore)
	}

	if err = c.handle(context.TODO(), restore); err != nil {
		logrus.Errorf("%s: %s/%s: %s", reflect.TypeOf(c), restore.Namespace, restore.Name, err)
		return reconcile.Result{RequeueAfter: controllers.DefaultRequeueError}, err
	}

	return reconcile.Result{RequeueAfter: controllers.DefaultRequeue}, nil
}

// Handle updates for SnapshotRestore objects
func (c *SnapshotRestoreController) handle(ctx context.Context, snapRestore *stork_api.VolumeSnapshotRestore) error {
	if snapRestore.DeletionTimestamp != nil {
		if controllers.ContainsFinalizer(snapRestore, controllers.FinalizerCleanup) {
			if err := c.handleDelete(snapRestore); err != nil {
				logrus.Errorf("%s: cleanup: %s", reflect.TypeOf(c), err)
			}
		}

		if snapRestore.GetFinalizers() != nil {
			controllers.RemoveFinalizer(snapRestore, controllers.FinalizerCleanup)
			return c.client.Update(ctx, snapRestore)
		}

		return nil
	}

	var err error
	switch snapRestore.Status.Status {
	case stork_api.VolumeSnapshotRestoreStatusInitial:
		err = c.handleInitial(snapRestore)
	case stork_api.VolumeSnapshotRestoreStatusPending,
		stork_api.VolumeSnapshotRestoreStatusInProgress:
		err = c.handleStartRestore(snapRestore)
	case stork_api.VolumeSnapshotRestoreStatusStaged:
		err = c.handleFinal(snapRestore)
		if err == nil {
			c.recorder.Event(snapRestore,
				v1.EventTypeNormal,
				string(snapRestore.Status.Status),
				"Snapshot in-Place  Restore completed")
		}
	case stork_api.VolumeSnapshotRestoreStatusFailed:
		err = c.volDriver.CleanupSnapshotRestoreObjects(snapRestore)
	case stork_api.VolumeSnapshotRestoreStatusSuccessful:
		return nil
	default:
		err = fmt.Errorf("invalid stage for volume snapshot restore: %v", snapRestore.Status.Status)
	}

	if err != nil {
		log.VolumeSnapshotRestoreLog(snapRestore).Errorf("Error handling event: %v err: %v", snapRestore, err.Error())
		c.recorder.Event(snapRestore,
			v1.EventTypeWarning,
			string(stork_api.VolumeSnapshotRestoreStatusFailed),
			err.Error())
	}

	err = c.client.Update(context.TODO(), snapRestore)
	if err != nil {
		return err
	}

	return nil
}

func (c *SnapshotRestoreController) handleStartRestore(snapRestore *stork_api.VolumeSnapshotRestore) error {
	log.VolumeSnapshotRestoreLog(snapRestore).Infof("Preparing volumes for snapshot restore %v", snapRestore.Spec.SourceName)
	inProgress, err := c.waitForRestoreToReady(snapRestore)
	if err != nil {
		return err
	}
	if inProgress {
		snapRestore.Status.Status = stork_api.VolumeSnapshotRestoreStatusInProgress
		return nil
	}

	// start in-place restore
	snapRestore.Status.Status = stork_api.VolumeSnapshotRestoreStatusStaged
	return nil
}

func (c *SnapshotRestoreController) handleInitial(snapRestore *stork_api.VolumeSnapshotRestore) error {
	// snapshot is list of snapshots
	snapshotList := []*snap_v1.VolumeSnapshot{}
	var err error

	snapName := snapRestore.Spec.SourceName
	snapNamespace := snapRestore.Spec.SourceNamespace
	log.VolumeSnapshotRestoreLog(snapRestore).Infof("Starting in place restore for snapshot %v", snapName)
	if snapRestore.Spec.GroupSnapshot {
		log.VolumeSnapshotRestoreLog(snapRestore).Infof("GroupVolumeSnapshot In-place restore request for %v", snapName)
		snapshotList, err = storkops.Instance().GetSnapshotsForGroupSnapshot(snapName, snapNamespace)
		if err != nil {
			log.VolumeSnapshotRestoreLog(snapRestore).Errorf("unable to get group snapshot details %v", err)
			return err
		}
	} else {
		// GetSnapshot Details
		snapshot, err := k8sextops.Instance().GetSnapshot(snapName, snapNamespace)
		if err != nil {
			return fmt.Errorf("unable to get get snapshot  details %s: %v",
				snapName, err)
		}
		if err := k8sextops.Instance().ValidateSnapshot(snapName,
			snapNamespace, false,
			validateSnapshotRetryTimeout,
			validateSnapshotTimeout); err != nil {
			return fmt.Errorf("snapshot is not complete %v", err)
		}
		snapshotList = append(snapshotList, snapshot)
	}

	// get map of snapID and pvcs
	err = initRestoreVolumesInfo(snapshotList, snapRestore)
	if err != nil {
		return err
	}

	snapRestore.Status.Status = stork_api.VolumeSnapshotRestoreStatusPending
	return nil
}

func (c *SnapshotRestoreController) handleFinal(snapRestore *stork_api.VolumeSnapshotRestore) error {
	var err error

	// annotate and delete pods using pvcs
	err = markPVCForRestore(snapRestore.Status.Volumes)
	if err != nil {
		log.VolumeSnapshotRestoreLog(snapRestore).Errorf("unable to mark pvc for restore %v", err)
		return err
	}
	// Do driver volume snapshot restore here
	err = c.volDriver.CompleteVolumeSnapshotRestore(snapRestore)
	if err != nil {
		if err := unmarkPVCForRestore(snapRestore.Status.Volumes); err != nil {
			log.VolumeSnapshotRestoreLog(snapRestore).Errorf("unable to umark pvc for restore %v", err)
			return err
		}
		snapRestore.Status.Status = stork_api.VolumeSnapshotRestoreStatusFailed
		return fmt.Errorf("failed to restore pvc %v", err)
	}
	err = unmarkPVCForRestore(snapRestore.Status.Volumes)
	if err != nil {
		log.VolumeSnapshotRestoreLog(snapRestore).Errorf("unable to unmark pvc for restore %v", err)
		return err
	}

	snapRestore.Status.Status = stork_api.VolumeSnapshotRestoreStatusSuccessful
	return nil
}

func markPVCForRestore(volumes []*stork_api.RestoreVolumeInfo) error {
	// Get a list of pods that need to be deleted
	for _, vol := range volumes {
		pvc, err := core.Instance().GetPersistentVolumeClaim(vol.PVC, vol.Namespace)
		if err != nil {
			return fmt.Errorf("failed to get pvc details %v", err)
		}
		if pvc.Annotations == nil {
			pvc.Annotations = make(map[string]string)
		}
		pvc.Annotations[RestoreAnnotation] = "true"
		newPvc, err := core.Instance().UpdatePersistentVolumeClaim(pvc)
		if err != nil {
			return err
		}
		pods, err := core.Instance().GetPodsUsingPVC(newPvc.Name, newPvc.Namespace)
		if err != nil {
			return err
		}
		for _, pod := range pods {
			if pod.Spec.SchedulerName != storkSchedulerName {
				return fmt.Errorf("application not scheduled by stork scheduler")
			}
		}

		logrus.Infof("Deleting pods using volume %v/%v", vol.PVC, vol.Namespace)
		if err := ensurePodsDeletion(pods); err != nil {
			logrus.Errorf("Failed to delete pods using volume %v/%v: %v", vol.PVC, vol.Namespace, err)
			return err
		}
	}
	return nil
}

func ensurePodsDeletion(pods []v1.Pod) error {
	if err := core.Instance().DeletePods(pods, false); err != nil {
		return err
	}
	var (
		wg               sync.WaitGroup
		podDeleteErr     error
		podDeleteErrLock sync.Mutex
	)
	wg.Add(len(pods))
	for _, p := range pods {
		podDeleteFunc := func(pod v1.Pod) {
			defer wg.Done()
			if err := core.Instance().WaitForPodDeletion(pod.UID, pod.Namespace, 120*time.Second); err != nil {
				log.PodLog(&pod).Errorf("Pod is not deleted %v:%v", pod.Name, err)
				// Force delete the pod
				if err := core.Instance().DeletePod(pod.Name, pod.Namespace, true); err != nil {
					log.PodLog(&pod).Errorf("Error force deleting pod %v: %v", pod.Name, err)
					podDeleteErrLock.Lock()
					podDeleteErr = multierror.Append(podDeleteErr, err)
					podDeleteErrLock.Unlock()
					return
				}
				// wait for a shorter period of time since this was a force delete
				if err := core.Instance().WaitForPodDeletion(pod.UID, pod.Namespace, 30*time.Second); err != nil {
					log.PodLog(&pod).Errorf("Failed to forcefully delete pods %v: %v", pod.Name, err)
					podDeleteErrLock.Lock()
					podDeleteErr = multierror.Append(podDeleteErr, err)
					podDeleteErrLock.Unlock()
				}
			}
			// wait for pod deletion
			log.PodLog(&pod).Debugf("Deleted pod %v", pod.Name)
		}
		go podDeleteFunc(p)
	}
	// Wait for all goroutines to finish
	wg.Wait()
	return podDeleteErr
}

func unmarkPVCForRestore(volumes []*stork_api.RestoreVolumeInfo) error {
	// remove annotation from pvc's
	for _, vol := range volumes {
		pvc, err := core.Instance().GetPersistentVolumeClaim(vol.PVC, vol.Namespace)
		if err != nil {
			return fmt.Errorf("failed to get pvc details %v", err)
		}
		logrus.Infof("Removing annotation for %v", pvc.Name)
		if pvc.Annotations == nil {
			// somehow annotation got deleted but since restore is done,
			// we shouldn't care
			log.PVCLog(pvc).Warnf("No annotation found for %v", pvc.Name)
			continue
		}
		if _, ok := pvc.Annotations[RestoreAnnotation]; !ok {
			log.PVCLog(pvc).Warnf("Restore annotation not found for %v", pvc.Name)
			continue
		}
		delete(pvc.Annotations, RestoreAnnotation)
		_, err = core.Instance().UpdatePersistentVolumeClaim(pvc)
		if err != nil {
			log.PVCLog(pvc).Warnf("failed to update pvc %v", err)
			return err
		}
	}

	return nil
}

func initRestoreVolumesInfo(snapshotList []*snap_v1.VolumeSnapshot, snapRestore *stork_api.VolumeSnapshotRestore) error {
	for _, snap := range snapshotList {
		snapData := string(snap.Spec.SnapshotDataName)
		logrus.Debugf("Getting volume ID for pvc %v", snap.Spec.PersistentVolumeClaimName)
		pvc, err := core.Instance().GetPersistentVolumeClaim(snap.Spec.PersistentVolumeClaimName, snap.Metadata.Namespace)
		if err != nil {
			return fmt.Errorf("failed to get pvc details for snapshot %v", err)
		}
		volInfo := &stork_api.RestoreVolumeInfo{}
		// check whether we have volInfo already processed for given
		// pvc. If so update existing vol info
		isPresent := false
		for _, vol := range snapRestore.Status.Volumes {
			if pvc.Name == vol.PVC {
				volInfo = vol
				isPresent = true
				break
			}
		}
		if !isPresent {
			volInfo.Volume = pvc.Spec.VolumeName
			volInfo.PVC = pvc.Name
			volInfo.Namespace = pvc.Namespace
			volInfo.Snapshot = snapData
			volInfo.RestoreStatus = stork_api.VolumeSnapshotRestoreStatusInitial
			snapRestore.Status.Volumes = append(snapRestore.Status.Volumes, volInfo)
		}
	}
	return nil
}

func (c *SnapshotRestoreController) createCRD() error {
	resource := apiextensions.CustomResource{
		Name:    stork_api.SnapshotRestoreResourceName,
		Plural:  stork_api.SnapshotRestoreResourcePlural,
		Group:   stork_api.SchemeGroupVersion.Group,
		Version: stork_api.SchemeGroupVersion.Version,
		Scope:   apiextensionsv1beta1.NamespaceScoped,
		Kind:    reflect.TypeOf(stork_api.VolumeSnapshotRestore{}).Name(),
	}
	ok, err := version.RequiresV1Registration()
	if err != nil {
		return err
	}
	if ok {
		err := k8sutils.CreateCRD(resource)
		if err != nil && !errors.IsAlreadyExists(err) {
			return err
		}
		return apiextensions.Instance().ValidateCRD(resource.Plural+"."+resource.Group, validateCRDTimeout, validateCRDInterval)
	}
	err = apiextensions.Instance().CreateCRDV1beta1(resource)
	if err != nil && !errors.IsAlreadyExists(err) {
		return err
	}
	return apiextensions.Instance().ValidateCRDV1beta1(resource, validateCRDTimeout, validateCRDInterval)
}

func (c *SnapshotRestoreController) handleDelete(snapRestore *stork_api.VolumeSnapshotRestore) error {
	return c.volDriver.CleanupSnapshotRestoreObjects(snapRestore)
}

func (c *SnapshotRestoreController) waitForRestoreToReady(
	snapRestore *stork_api.VolumeSnapshotRestore,
) (bool, error) {
	if snapRestore.Status.Status == stork_api.VolumeSnapshotRestoreStatusPending {
		err := c.volDriver.StartVolumeSnapshotRestore(snapRestore)
		if err != nil {
			message := fmt.Sprintf("Error starting snapshot restore for volumes: %v", err)
			log.VolumeSnapshotRestoreLog(snapRestore).Errorf(message)
			c.recorder.Event(snapRestore,
				v1.EventTypeWarning,
				string(stork_api.VolumeSnapshotRestoreStatusFailed),
				message)
			return false, err
		}

		snapRestore.Status.Status = stork_api.VolumeSnapshotRestoreStatusInProgress
		err = c.client.Update(context.TODO(), snapRestore)
		if err != nil {
			return false, err
		}
	}

	// Volume Snapshot restore is already initiated , check for status
	continueProcessing := false
	// Skip checking status if no volumes are being restored
	if len(snapRestore.Status.Volumes) != 0 {
		err := c.volDriver.GetVolumeSnapshotRestoreStatus(snapRestore)
		if err != nil {
			return continueProcessing, err
		}

		// Now check if there is any failure or success
		for _, vInfo := range snapRestore.Status.Volumes {
			if vInfo.RestoreStatus == stork_api.VolumeSnapshotRestoreStatusInProgress {
				log.VolumeSnapshotRestoreLog(snapRestore).Infof("Volume restore for volume %v is in %v state", vInfo.PVC, vInfo.RestoreStatus)
				continueProcessing = true
			} else if vInfo.RestoreStatus == stork_api.VolumeSnapshotRestoreStatusFailed {
				c.recorder.Event(snapRestore,
					v1.EventTypeWarning,
					string(vInfo.RestoreStatus),
					fmt.Sprintf("Error restoring volume %v: %v", vInfo.PVC, vInfo.Reason))
				return false, fmt.Errorf("restore failed for volume: %v", vInfo.PVC)
			} else if vInfo.RestoreStatus == stork_api.VolumeSnapshotRestoreStatusSuccessful {
				c.recorder.Event(snapRestore,
					v1.EventTypeNormal,
					string(vInfo.RestoreStatus),
					fmt.Sprintf("Volume %v restored successfully", vInfo.PVC))
			}
		}
	}

	return continueProcessing, nil
}
