/*
Copyright 2021.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"
	"fmt"
	cmapi "github.com/jetstack/cert-manager/pkg/apis/certmanager/v1alpha3"
	"github.com/jniebuhr/aws-pca-issuer/pkg/aws"
	"github.com/jniebuhr/aws-pca-issuer/pkg/util"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"

	"github.com/go-logr/logr"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cmutil "github.com/jetstack/cert-manager/pkg/api/util"
	cmmeta "github.com/jetstack/cert-manager/pkg/apis/meta/v1"
	api "github.com/jniebuhr/aws-pca-issuer/pkg/api/v1beta1"
)

// CertificateRequestReconciler reconciles a AWSPCAIssuer object
type CertificateRequestReconciler struct {
	client.Client
	Log      logr.Logger
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=cert-manager.io,resources=certificaterequests,verbs=get;list;watch;update
// +kubebuilder:rbac:groups=cert-manager.io,resources=certificaterequests/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.7.0/pkg/reconcile
func (r *CertificateRequestReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("certificaterequest", req.NamespacedName)
	cr := new(cmapi.CertificateRequest)
	if err := r.Client.Get(ctx, req.NamespacedName, cr); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}

		log.Error(err, "Failed to request CertificateRequest")
		return ctrl.Result{}, err
	}

	if cr.Spec.IssuerRef.Group != "" && cr.Spec.IssuerRef.Group != api.GroupVersion.Group {
		log.V(4).Info("CertificateRequest does not specify an issuerRef matching our group")
		return ctrl.Result{}, nil
	}

	if len(cr.Status.Certificate) > 0 {
		log.V(4).Info("Certificate was already signed")
		return ctrl.Result{}, nil
	}

	if cr.Spec.IsCA {
		log.Info("AWSPCA does not support CA certificates")
		return ctrl.Result{}, nil
	}

	issuerName := types.NamespacedName{
		Namespace: cr.Namespace,
		Name:      cr.Spec.IssuerRef.Name,
	}
	if cr.Spec.IssuerRef.Kind == "AWSPCAClusterIssuer" {
		issuerName.Namespace = ""
	}

	iss, err := util.GetIssuer(ctx, r.Client, issuerName)
	if err != nil {
		log.Error(err, "failed to retrieve Issuer resource")
		_ = r.setStatus(ctx, cr, cmmeta.ConditionFalse, cmapi.CertificateRequestReasonFailed, "issuer could not be found")
		return ctrl.Result{}, err
	}

	if !isReady(iss) {
		err := fmt.Errorf("issuer %s is not ready", iss.GetName())
		_ = r.setStatus(ctx, cr, cmmeta.ConditionFalse, cmapi.CertificateRequestReasonFailed, "issuer is not ready")
		return ctrl.Result{}, err
	}

	provisioner, ok := aws.GetProvisioner(issuerName)
	if !ok {
		err := fmt.Errorf("provisioner for %s not found", issuerName)
		log.Error(err, "failed to retrieve provisioner")
		_ = r.setStatus(ctx, cr, cmmeta.ConditionFalse, cmapi.CertificateRequestReasonFailed, "failed to retrieve provisioner")
		return ctrl.Result{}, err
	}

	pem, ca, err := provisioner.Sign(ctx, cr)
	if err != nil {
		log.Error(err, "failed to request certificate from PCA")
		return ctrl.Result{}, r.setStatus(ctx, cr, cmmeta.ConditionFalse, cmapi.CertificateRequestReasonFailed, "failed to request certificate from PCA")
	}
	cr.Status.Certificate = pem
	cr.Status.CA = ca

	return ctrl.Result{}, r.setStatus(ctx, cr, cmmeta.ConditionTrue, cmapi.CertificateRequestReasonIssued, "certificate issued")
}

// SetupWithManager sets up the controller with the Manager.
func (r *CertificateRequestReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&cmapi.CertificateRequest{}).
		Complete(r)
}

func isReady(issuer api.GenericIssuer) bool {
	for _, condition := range issuer.GetStatus().Conditions {
		if condition.Type == api.ConditionTypeReady && condition.Status == v1.ConditionTrue {
			return true
		}
	}
	return false
}

func (r *CertificateRequestReconciler) setStatus(ctx context.Context, cr *cmapi.CertificateRequest, status cmmeta.ConditionStatus, reason, message string, args ...interface{}) error {
	completeMessage := fmt.Sprintf(message, args...)
	SetCertificateRequestCondition(cr, "Ready", status, reason, completeMessage, r.Log)

	eventType := core.EventTypeNormal
	if status == cmmeta.ConditionFalse {
		eventType = core.EventTypeWarning
	}
	r.Recorder.Event(cr, eventType, reason, completeMessage)

	return r.Client.Status().Update(ctx, cr)
}

func SetCertificateRequestCondition(cr *cmapi.CertificateRequest, conditionType cmapi.CertificateRequestConditionType, status cmmeta.ConditionStatus, reason, message string, log logr.Logger) {
	newCondition := cmapi.CertificateRequestCondition{
		Type:    conditionType,
		Status:  status,
		Reason:  reason,
		Message: message,
	}

	nowTime := metav1.NewTime(cmutil.Clock.Now())
	newCondition.LastTransitionTime = &nowTime

	// Search through existing conditions
	for idx, cond := range cr.Status.Conditions {
		// Skip unrelated conditions
		if cond.Type != conditionType {
			continue
		}

		// If this update doesn't contain a state transition, we don't update
		// the conditions LastTransitionTime to Now()
		if cond.Status == status {
			newCondition.LastTransitionTime = cond.LastTransitionTime
		} else {
			log.V(4).Info("Found status change for CertificateRequest %q condition %q: %q -> %q; setting lastTransitionTime to %v", cr.Name, conditionType, cond.Status, status, nowTime.Time)
		}

		// Overwrite the existing condition
		cr.Status.Conditions[idx] = newCondition
		return
	}

	// If we've not found an existing condition of this type, we simply insert
	// the new condition into the slice.
	cr.Status.Conditions = append(cr.Status.Conditions, newCondition)
	log.V(4).Info("Setting lastTransitionTime for CertificateRequest %q condition %q to %v", cr.Name, conditionType, nowTime.Time)
}
