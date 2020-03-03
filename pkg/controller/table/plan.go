package table

import (
	"bytes"
	"context"
	"io"
	"strings"
	"time"

	"github.com/pkg/errors"
	databasesv1alpha3 "github.com/schemahero/schemahero/pkg/apis/databases/v1alpha3"
	schemasv1alpha3 "github.com/schemahero/schemahero/pkg/apis/schemas/v1alpha3"
	schemasclientv1alpha3 "github.com/schemahero/schemahero/pkg/client/schemaheroclientset/typed/schemas/v1alpha3"
	"github.com/schemahero/schemahero/pkg/logger"
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	kuberneteserrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func (r *ReconcileTable) reconcilePod(pod *corev1.Pod) (reconcile.Result, error) {
	podLabels := pod.GetObjectMeta().GetLabels()
	role, ok := podLabels["schemahero-role"]
	if !ok {
		return reconcile.Result{}, nil
	}

	logger.Debug("reconciling schemahero pod",
		zap.String("kind", pod.Kind),
		zap.String("name", pod.Name),
		zap.String("role", role),
		zap.String("podPhase", string(pod.Status.Phase)))

	if role != "table" && role != "plan" {
		// we want to avoid migration pods in this reconciler
		return reconcile.Result{}, nil
	}

	if pod.Status.Phase != corev1.PodSucceeded {
		return reconcile.Result{}, nil
	}

	// Write the plan from stdout to the object itself
	cfg, err := config.GetConfig()
	if err != nil {
		return reconcile.Result{}, errors.Wrap(err, "failed to get config")
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return reconcile.Result{}, errors.Wrap(err, "failed to create client")
	}

	podLogOpts := corev1.PodLogOptions{}
	req := client.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, &podLogOpts)
	podLogs, err := req.Stream()
	if err != nil {
		return reconcile.Result{}, errors.Wrap(err, "failed to open log stream")
	}
	defer podLogs.Close()

	buf := new(bytes.Buffer)
	_, err = io.Copy(buf, podLogs)
	if err != nil {
		return reconcile.Result{}, errors.Wrap(err, "failed to copy logs too buffer")
	}

	out := buf.String()

	// remove empty lines from output
	// the planner plans each row, and can leave empty lines
	out = strings.Replace(out, "\n\n", "\n", -1)

	logger.Debug("read output from pod",
		zap.String("kind", pod.Kind),
		zap.String("name", pod.Name),
		zap.String("role", role),
		zap.String("output", out))

	tableName, ok := podLabels["schemahero-name"]
	if !ok {
		return reconcile.Result{}, nil
	}
	tableNamespace, ok := podLabels["schemahero-namespace"]
	if !ok {
		return reconcile.Result{}, nil
	}

	schemasClient, err := schemasclientv1alpha3.NewForConfig(cfg)
	if err != nil {
		return reconcile.Result{}, errors.Wrap(err, "failed to create schema client")
	}

	table, err := schemasClient.Tables(tableNamespace).Get(tableName, metav1.GetOptions{})
	if err != nil {
		return reconcile.Result{}, errors.Wrap(err, "failed to get existing table")
	}
	tableSHA, err := table.GetSHA()
	if err != nil {
		return reconcile.Result{}, errors.Wrap(err, "failed to get sha of table")
	}

	migration := schemasv1alpha3.Migration{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "schemas.schemahero.io/v1alpha3",
			Kind:       "Migration",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      tableSHA,
			Namespace: table.Namespace,
		},
		Spec: schemasv1alpha3.MigrationSpec{
			GeneratedDDL: out,

			TableName:      table.Name,
			TableNamespace: table.Namespace,

			DatabaseName: table.Spec.Database,
		},
		Status: schemasv1alpha3.MigrationStatus{
			PlannedAt: time.Now().Unix(),
		},
	}

	if err := controllerutil.SetControllerReference(table, &migration, r.scheme); err != nil {
		return reconcile.Result{}, errors.Wrap(err, "failed to set owner ref of migration")
	}

	if err := r.Create(context.Background(), &migration); err != nil {
		return reconcile.Result{}, errors.Wrap(err, "failed to create migration resource")
	}

	// Delete the pod and config map
	if err := r.Delete(context.Background(), pod); err != nil {
		return reconcile.Result{}, errors.Wrap(err, "failed to delete pod from plan phase")
	}

	configMapName := ""
	for _, volume := range pod.Spec.Volumes {
		if volume.Name == "specs" && volume.ConfigMap != nil {
			configMapName = volume.ConfigMap.Name
		}
	}
	configMap := corev1.ConfigMap{}
	err = r.Get(context.Background(), types.NamespacedName{Name: configMapName, Namespace: pod.Namespace}, &configMap)
	if err != nil {
		return reconcile.Result{}, errors.Wrap(err, "failed to get config map from plan phase")
	}

	if err := r.Delete(context.Background(), &configMap); err != nil {
		return reconcile.Result{}, errors.Wrap(err, "failed to delete config map from plan phase")
	}

	return reconcile.Result{}, nil
}

func (r *ReconcileTable) plan(database *databasesv1alpha3.Database, table *schemasv1alpha3.Table) error {
	logger.Debug("deploying plan")

	configMap, err := r.planConfigMap(database, table)
	if err != nil {
		return errors.Wrap(err, "failed to get config map object for plan")
	}
	pod, err := r.planPod(database, table)
	if err != nil {
		return errors.Wrap(err, "failed to get pod for plan")
	}

	if err := r.ensureTableConfigMap(configMap); err != nil {
		return errors.Wrap(err, "failed to create config map for plan")
	}

	if err := r.ensureTablePod(pod); err != nil {
		return errors.Wrap(err, "failerd to create pod for plan")
	}

	return nil
}

func (r *ReconcileTable) readConnectionURI(namespace string, valueOrValueFrom databasesv1alpha3.ValueOrValueFrom) (string, error) {
	if valueOrValueFrom.Value != "" {
		return valueOrValueFrom.Value, nil
	}

	if valueOrValueFrom.ValueFrom == nil {
		return "", errors.New("value and valueFrom cannot both be nil/empty")
	}

	if valueOrValueFrom.ValueFrom.SecretKeyRef != nil {
		secret := &corev1.Secret{}
		secretNamespacedName := types.NamespacedName{
			Name:      valueOrValueFrom.ValueFrom.SecretKeyRef.Name,
			Namespace: namespace,
		}

		if err := r.Get(context.Background(), secretNamespacedName, secret); err != nil {
			if kuberneteserrors.IsNotFound(err) {
				return "", errors.New("table secret not found")
			} else {
				return "", errors.Wrap(err, "failed to get existing connection secret")
			}
		}

		return string(secret.Data[valueOrValueFrom.ValueFrom.SecretKeyRef.Key]), nil
	}

	return "", errors.New("unable to find supported valueFrom")
}
