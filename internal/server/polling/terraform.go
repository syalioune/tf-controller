package polling

import (
	"context"
	"fmt"
	"time"

	"github.com/fluxcd/pkg/runtime/acl"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	bpconfig "github.com/weaveworks/tf-controller/internal/config"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sLabels "k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	infrav1 "github.com/weaveworks/tf-controller/api/v1alpha2"
)

func (s *Server) getTerraformObject(ctx context.Context, ref client.ObjectKey) (*infrav1.Terraform, error) {
	obj := &infrav1.Terraform{}
	if err := s.clusterClient.Get(ctx, ref, obj); err != nil {
		return nil, fmt.Errorf("unable to get Terraform: %w", err)
	}

	return obj, nil
}

func (s *Server) listTerraformObjects(ctx context.Context, namespace string, labels map[string]string) ([]*infrav1.Terraform, error) {
	tfList := &infrav1.TerraformList{}

	opts := []client.ListOption{client.InNamespace(namespace)}

	if labels != nil {
		opts = append(opts, client.MatchingLabelsSelector{
			Selector: k8sLabels.Set(labels).AsSelector(),
		})
	}

	if err := s.clusterClient.List(ctx, tfList, opts...); err != nil {
		return nil, fmt.Errorf("unable to list Terraform objects: %w", err)
	}

	result := make([]*infrav1.Terraform, len(tfList.Items))
	for i := range tfList.Items {
		result[i] = &tfList.Items[i]
	}

	return result, nil
}

func (s *Server) getSource(ctx context.Context, tf *infrav1.Terraform) (*sourcev1.GitRepository, error) {
	if tf.Spec.SourceRef.Kind != sourcev1.GitRepositoryKind {
		return nil, fmt.Errorf("branch based planner does not support source kind: %s", tf.Spec.SourceRef.Kind)
	}

	ref := client.ObjectKey{
		Namespace: tf.GetNamespace(),
		Name:      tf.Spec.SourceRef.Name,
	}

	if ns := tf.Spec.SourceRef.Namespace; ns != "" {
		ref.Namespace = ns
	}

	if s.noCrossNamespaceRefs && ref.Namespace != tf.GetNamespace() {
		return nil, acl.AccessDeniedError(
			fmt.Sprintf("cannot access %s/%s, cross-namespace references have been disabled", tf.Spec.SourceRef.Kind, ref),
		)
	}

	obj := &sourcev1.GitRepository{}
	if err := s.clusterClient.Get(ctx, ref, obj); err != nil {
		return nil, fmt.Errorf("unable to get Source: %w", err)
	}

	return obj, nil
}

func (s *Server) reconcileTerraform(ctx context.Context, originalTF *infrav1.Terraform, originalSource *sourcev1.GitRepository, branch string, prID string, interval time.Duration) error {
	tfName := s.createObjectName(originalTF.Name, branch, prID)
	msg := fmt.Sprintf("Terraform object %s in the namespace %s", tfName, originalTF.Namespace)
	source, err := s.reconcileSource(ctx, originalSource, branch, prID, interval)
	if err != nil {
		return fmt.Errorf("unable to reconcile Source for %s: %w", msg, err)
	}

	tf := &infrav1.Terraform{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tfName,
			Namespace: originalTF.Namespace,
		},
	}

	branchLabels := s.createLabels(originalTF.Labels, originalTF.Name, branch, prID)
	op, err := controllerutil.CreateOrUpdate(ctx, s.clusterClient, tf, func() error {
		spec := originalTF.Spec.DeepCopy()

		spec.SourceRef.Name = source.Name
		spec.SourceRef.Namespace = source.Namespace
		spec.PlanOnly = true
		spec.StoreReadablePlan = "human"
		// relocate the output secret, so it's not shared between branches
		if spec.WriteOutputsToSecret != nil && originalTF.Spec.WriteOutputsToSecret != nil {
			spec.WriteOutputsToSecret.Name = s.createObjectName(originalTF.Spec.WriteOutputsToSecret.Name, branch, prID)
		}
		spec.ApprovePlan = ""
		spec.Force = false

		tf.Spec = *spec

		tf.SetLabels(branchLabels)

		return nil
	})
	if err != nil {
		return fmt.Errorf("reconcile failed for %s: %w", msg, err)
	} else if op != controllerutil.OperationResultNone {
		s.log.Info(fmt.Sprintf("%s successfully reconciled", msg), "operation", op)
	}

	return nil
}

func (s *Server) reconcileSource(ctx context.Context, originalSource *sourcev1.GitRepository, branch string, prID string, interval time.Duration) (*sourcev1.GitRepository, error) {
	sourceName := s.createObjectName(originalSource.Name, branch, prID)
	msg := fmt.Sprintf("Source %s in the namespace %s", sourceName, originalSource.Namespace)
	source := &sourcev1.GitRepository{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sourceName,
			Namespace: originalSource.Namespace,
		},
		Spec: originalSource.Spec,
	}
	branchLabels := s.createLabels(originalSource.Labels, originalSource.Name, branch, prID)

	op, err := controllerutil.CreateOrUpdate(ctx, s.clusterClient, source, func() error {
		source.SetLabels(branchLabels)

		spec := originalSource.Spec.DeepCopy()

		spec.Reference.Branch = branch
		spec.Interval = metav1.Duration{
			Duration: interval,
		}

		source.Spec = *spec

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("reconcile failed for %s: %w", msg, err)
	} else if op != controllerutil.OperationResultNone {
		s.log.Info(fmt.Sprintf("%s successfully reconciled", msg), "operation", op)
	}

	return source, nil
}

func (s *Server) createObjectName(name string, branch string, prID string) string {
	return fmt.Sprintf("%s-%s-%s", name, branch, prID)
}

func (s *Server) createLabels(labels map[string]string, originalName string, branch string, prID string) map[string]string {
	if labels == nil {
		labels = make(map[string]string)
	}

	labels[bpconfig.LabelKey] = bpconfig.LabelValue
	labels[bpconfig.LabelPrimaryResourceKey] = originalName
	labels[bpconfig.LabelPRIDKey] = prID

	return labels
}

func (s *Server) deleteTerraform(ctx context.Context, tf *infrav1.Terraform) error {
	msg := fmt.Sprintf("Terraform %s in the namespace %s", tf.Name, tf.Namespace)

	if err := s.deleteSource(ctx, tf); err != nil {
		s.log.Error(err, fmt.Sprintf("unable to delete Source for %s", msg))
	}

	if err := s.clusterClient.Delete(ctx, tf); err != nil {
		return fmt.Errorf("unable to delete %s: %w", msg, err)
	}

	s.log.Info(fmt.Sprintf("deleted %s", msg))

	return nil
}

func (s *Server) deleteSource(ctx context.Context, tf *infrav1.Terraform) error {
	source, err := s.getSource(ctx, tf)
	if err != nil {
		return fmt.Errorf("unable to get Source for Terraform %s in the namespace %s: %w", tf.Name, tf.Namespace, err)
	}

	msg := fmt.Sprintf("Source %s in the namespace %s", source.Name, source.Namespace)

	if err := s.clusterClient.Delete(ctx, source); err != nil {
		return fmt.Errorf("unable to delete %s: %w", msg, err)
	}

	s.log.Info(fmt.Sprintf("deleted %s", msg))

	return nil
}

func (s *Server) getSecret(ctx context.Context, ref client.ObjectKey) (*corev1.Secret, error) {
	obj := &corev1.Secret{}
	if err := s.clusterClient.Get(ctx, ref, obj); err != nil {
		return nil, fmt.Errorf("unable to get Secret: %w", err)
	}

	return obj, nil
}
