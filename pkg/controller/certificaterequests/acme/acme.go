/*
Copyright 2020 The cert-manager Authors.

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

package acme

import (
	"context"
	"crypto/x509"
	"fmt"

	k8sErrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"

	"github.com/jetstack/cert-manager/pkg/acme"
	apiutil "github.com/jetstack/cert-manager/pkg/api/util"
	cmacme "github.com/jetstack/cert-manager/pkg/apis/acme/v1"
	v1 "github.com/jetstack/cert-manager/pkg/apis/certmanager/v1"
	cmacmeclientset "github.com/jetstack/cert-manager/pkg/client/clientset/versioned/typed/acme/v1"
	cmacmelisters "github.com/jetstack/cert-manager/pkg/client/listers/acme/v1"
	controllerpkg "github.com/jetstack/cert-manager/pkg/controller"
	"github.com/jetstack/cert-manager/pkg/controller/certificaterequests"
	crutil "github.com/jetstack/cert-manager/pkg/controller/certificaterequests/util"
	issuerpkg "github.com/jetstack/cert-manager/pkg/issuer"
	logf "github.com/jetstack/cert-manager/pkg/logs"
	"github.com/jetstack/cert-manager/pkg/util"
	"github.com/jetstack/cert-manager/pkg/util/pki"
)

const (
	CRControllerName = "certificaterequests-issuer-acme"
)

type ACME struct {
	// used to record Events about resources to the API
	recorder      record.EventRecorder
	issuerOptions controllerpkg.IssuerOptions

	orderLister cmacmelisters.OrderLister
	acmeClientV cmacmeclientset.AcmeV1Interface

	reporter *crutil.Reporter
}

func init() {
	// create certificate request controller for acme issuer
	controllerpkg.Register(CRControllerName, func(ctx *controllerpkg.Context) (controllerpkg.Interface, error) {
		// watch owned Order resources and trigger resyncs of CertificateRequests
		// that own Orders automatically
		orderInformer := ctx.SharedInformerFactory.Acme().V1().Orders().Informer()
		return controllerpkg.NewBuilder(ctx, CRControllerName).
			For(certificaterequests.New(apiutil.IssuerACME, NewACME(ctx), orderInformer)).
			Complete()
	})
}

func NewACME(ctx *controllerpkg.Context) *ACME {
	return &ACME{
		recorder:      ctx.Recorder,
		issuerOptions: ctx.IssuerOptions,
		orderLister:   ctx.SharedInformerFactory.Acme().V1().Orders().Lister(),
		acmeClientV:   ctx.CMClient.AcmeV1(),
		reporter:      crutil.NewReporter(ctx.Clock, ctx.Recorder),
	}
}

func (a *ACME) Sign(ctx context.Context, cr *v1.CertificateRequest, issuer v1.GenericIssuer) (*issuerpkg.IssueResponse, error) {
	log := logf.FromContext(ctx, "sign")

	// If we can't decode the CSR PEM we have to hard fail
	csr, err := pki.DecodeX509CertificateRequestBytes(cr.Spec.Request)
	if err != nil {
		message := "Failed to decode CSR in spec.request"

		a.reporter.Failed(cr, err, "RequestParsingError", message)
		log.Error(err, message)

		return nil, nil
	}

	// If the CommonName is also not present in the DNS names or IP Addresses of the Request then hard fail.
	if len(csr.Subject.CommonName) > 0 && !util.Contains(csr.DNSNames, csr.Subject.CommonName) && !util.Contains(pki.IPAddressesToString(csr.IPAddresses), csr.Subject.CommonName) {
		err = fmt.Errorf("%q does not exist in %s or %s", csr.Subject.CommonName, csr.DNSNames, pki.IPAddressesToString(csr.IPAddresses))
		message := "The CSR PEM requests a commonName that is not present in the list of dnsNames or ipAddresses. If a commonName is set, ACME requires that the value is also present in the list of dnsNames or ipAddresses"

		a.reporter.Failed(cr, err, "InvalidOrder", message)

		log.V(logf.DebugLevel).Info(fmt.Sprintf("%s: %s", message, err))

		return nil, nil
	}

	// If we fail to build the order we have to hard fail.
	expectedOrder, err := buildOrder(cr, csr, issuer.GetSpec().ACME.EnableDurationFeature)
	if err != nil {
		message := "Failed to build order"

		a.reporter.Failed(cr, err, "OrderBuildingError", message)
		log.Error(err, message)

		return nil, nil
	}

	order, err := a.orderLister.Orders(expectedOrder.Namespace).Get(expectedOrder.Name)
	if k8sErrors.IsNotFound(err) {
		// Failing to create the order here is most likely network related.
		// We should backoff and keep trying.
		_, err = a.acmeClientV.Orders(expectedOrder.Namespace).Create(ctx, expectedOrder, metav1.CreateOptions{})
		if err != nil {
			message := fmt.Sprintf("Failed create new order resource %s/%s", expectedOrder.Namespace, expectedOrder.Name)

			a.reporter.Pending(cr, err, "OrderCreatingError", message)
			log.Error(err, message)

			return nil, err
		}

		message := fmt.Sprintf("Created Order resource %s/%s",
			expectedOrder.Namespace, expectedOrder.Name)
		a.reporter.Pending(cr, nil, "OrderCreated", message)
		log.V(logf.DebugLevel).Info(message)

		return nil, nil
	}
	if err != nil {
		// We are probably in a network error here so we should backoff and retry
		message := fmt.Sprintf("Failed to get order resource %s/%s", expectedOrder.Namespace, expectedOrder.Name)

		a.reporter.Pending(cr, err, "OrderGetError", message)
		log.Error(err, message)

		return nil, err
	}
	if !metav1.IsControlledBy(order, cr) {
		// TODO: improve this behaviour - this issue occurs because someone
		//  else may create a CertificateRequest with a name that is equal to
		//  the name of the request we are creating, due to our hash function
		//  not account for parameters stored on the Request (i.e. the public key).
		//  We should improve the way we hash input data or somehow avoid
		//  relying on deterministic names for Order resources.
		return nil, fmt.Errorf("found Order resource not owned by this CertificateRequest, retrying")
	}

	log = logf.WithRelatedResource(log, order)

	// If the acme order has failed then so too does the CertificateRequest meet the same fate.
	if acme.IsFailureState(order.Status.State) {
		message := fmt.Sprintf("Failed to wait for order resource %q to become ready", expectedOrder.Name)
		err := fmt.Errorf("order is in %q state: %s", order.Status.State, order.Status.Reason)
		a.reporter.Failed(cr, err, "OrderFailed", message)
		return nil, nil
	}

	if order.Status.State != cmacme.Valid {
		// We update here to just pending while we wait for the order to be resolved.
		a.reporter.Pending(cr, nil, "OrderPending",
			fmt.Sprintf("Waiting on certificate issuance from order %s/%s: %q",
				expectedOrder.Namespace, order.Name, order.Status.State))

		log.V(logf.DebugLevel).Info("acme Order resource is not in a ready state, waiting...")

		return nil, nil
	}

	if len(order.Status.Certificate) == 0 {
		a.reporter.Pending(cr, nil, "OrderPending",
			fmt.Sprintf("Waiting for order-controller to add certificate data to Order %s/%s",
				expectedOrder.Namespace, order.Name))

		log.V(logf.DebugLevel).Info("Order controller has not added certificate data to the Order, waiting...")
		return nil, nil
	}

	x509Cert, err := pki.DecodeX509CertificateBytes(order.Status.Certificate)
	if err != nil {
		log.Error(err, "failed to decode x509 certificate data on Order resource.")
		return nil, a.acmeClientV.Orders(order.Namespace).Delete(ctx, order.Name, metav1.DeleteOptions{})
	}

	if ok, err := pki.PublicKeyMatchesCertificate(csr.PublicKey, x509Cert); err != nil || !ok {
		log.Error(err, "The public key in Order.Status.Certificate does not match the public key in CertificateRequest.Spec.Request. Deleting the order.")
		return nil, a.acmeClientV.Orders(order.Namespace).Delete(ctx, order.Name, metav1.DeleteOptions{})
	}

	log.V(logf.InfoLevel).Info("certificate issued")

	// Order valid, return cert. The calling controller will update with ready if its happy with the cert.
	return &issuerpkg.IssueResponse{
		Certificate: order.Status.Certificate,
		CA:          order.Status.CA,
	}, nil

}

// Build order. If we error here it is a terminating failure.
func buildOrder(cr *v1.CertificateRequest, csr *x509.CertificateRequest, enableDurationFeature bool) (*cmacme.Order, error) {
	var ipAddresses []string
	for _, ip := range csr.IPAddresses {
		ipAddresses = append(ipAddresses, ip.String())
	}

	var dnsNames []string
	if csr.DNSNames != nil {
		dnsNames = csr.DNSNames
	}

	spec := cmacme.OrderSpec{
		Request:     cr.Spec.Request,
		IssuerRef:   cr.Spec.IssuerRef,
		CommonName:  csr.Subject.CommonName,
		DNSNames:    dnsNames,
		IPAddresses: ipAddresses,
	}

	if enableDurationFeature {
		spec.Duration = cr.Spec.Duration
	}

	computeNameSpec := spec.DeepCopy()
	// create a deep copy of the OrderSpec so we can overwrite the Request and NotAfter field
	computeNameSpec.Request = nil
	name, err := apiutil.ComputeName(cr.Name, computeNameSpec)
	if err != nil {
		return nil, err
	}

	// truncate certificate name so final name will be <= 63 characters.
	// hash (uint32) will be at most 10 digits long, and we account for
	// the hyphen.
	return &cmacme.Order{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   cr.Namespace,
			Labels:      cr.Labels,
			Annotations: cr.Annotations,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(cr, v1.SchemeGroupVersion.WithKind(v1.CertificateRequestKind)),
			},
		},
		Spec: spec,
	}, nil
}
