/*
Copyright 2023.

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

package analysis

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	metricsapi "github.com/keptn/lifecycle-toolkit/metrics-operator/api/v1alpha3"
	common "github.com/keptn/lifecycle-toolkit/metrics-operator/controllers/common/analysis"
	evalType "github.com/keptn/lifecycle-toolkit/metrics-operator/controllers/common/analysis/types"
	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/exp/maps"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

type Metrics struct {
	AnalysisResult  *prometheus.GaugeVec
	ObjectiveResult *prometheus.GaugeVec
}

// AnalysisReconciler reconciles an Analysis object
type AnalysisReconciler struct {
	client.Client
	Scheme     *runtime.Scheme
	Log        logr.Logger
	MaxWorkers int //maybe 2 or 4 as def
	Namespace  string
	NewWorkersPoolFactory
	common.IAnalysisEvaluator
	Metrics
}

//+kubebuilder:rbac:groups=metrics.keptn.sh,resources=analyses,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=metrics.keptn.sh,resources=analyses/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=metrics.keptn.sh,resources=analyses/finalizers,verbs=update
// +kubebuilder:rbac:groups=metrics.keptn.sh,resources=keptnmetricsproviders,verbs=get;list;watch;
//+kubebuilder:rbac:groups=metrics.keptn.sh,resources=analysisdefinitions,verbs=get;list;watch;
//+kubebuilder:rbac:groups=metrics.keptn.sh,resources=analysisvaluetemplates,verbs=get;list;watch;

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// For more details, check Reconcile and its AnalysisResult here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.13.0/pkg/reconcile
func (a *AnalysisReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	a.Log.Info("Reconciling Analysis")
	analysis := &metricsapi.Analysis{}

	//retrieve analysis
	if err := a.Client.Get(ctx, req.NamespacedName, analysis); err != nil {
		if errors.IsNotFound(err) {
			// taking down all associated K8s resources is handled by K8s
			a.Log.Info("Analysis resource not found. Ignoring since object must be deleted")
			return ctrl.Result{}, nil
		}
		a.Log.Error(err, "Failed to get the Analysis")
		return ctrl.Result{}, err
	}

	//find AnalysisDefinition to have the collection of Objectives
	analysisDef := &metricsapi.AnalysisDefinition{}
	if analysis.Spec.AnalysisDefinition.Namespace == "" {
		analysis.Spec.AnalysisDefinition.Namespace = a.Namespace
	}
	err := a.Client.Get(ctx,
		types.NamespacedName{
			Name:      analysis.Spec.AnalysisDefinition.Name,
			Namespace: analysis.Spec.AnalysisDefinition.Namespace},
		analysisDef,
	)

	if err != nil {
		if errors.IsNotFound(err) {
			a.Log.Info(
				fmt.Sprintf("AnalysisDefinition '%s' isn namespace '%s' not found, requeue",
					analysis.Spec.AnalysisDefinition.Name,
					analysis.Spec.AnalysisDefinition.Name),
			)
			return ctrl.Result{Requeue: true, RequeueAfter: 10 * time.Second}, nil
		}
		a.Log.Error(err, "Failed to retrieve the AnalysisDefinition")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	var done map[string]metricsapi.ProviderResult
	todo := analysisDef.Spec.Objectives
	if analysis.Status.StoredValues != nil {
		todo, done = extractMissingObjectives(analysisDef.Spec.Objectives, analysis.Status.StoredValues)
		if len(todo) == 0 {
			return ctrl.Result{}, nil
		}
	}

	//create multiple workers handling the Objectives
	childCtx, wp := a.NewWorkersPoolFactory(ctx, analysis, todo, a.MaxWorkers, a.Client, a.Log, a.Namespace)

	res, err := wp.DispatchAndCollect(childCtx)
	if err != nil {
		a.Log.Error(err, "Failed to collect all values required for the Analysis, caching collected values")
		analysis.Status.StoredValues = res
		err = a.updateStatus(ctx, analysis)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, err
	}

	maps.Copy(res, done)

	err = a.evaluateObjectives(ctx, res, analysisDef, analysis)

	return ctrl.Result{}, err
}

func (a *AnalysisReconciler) evaluateObjectives(ctx context.Context, res map[string]metricsapi.ProviderResult, analysisDef *metricsapi.AnalysisDefinition, analysis *metricsapi.Analysis) error {
	eval := a.Evaluate(res, analysisDef)
	analysisResultJSON, err := json.Marshal(eval)
	if err != nil {
		a.Log.Error(err, "Could not marshal status")
	} else {
		analysis.Status.Raw = string(analysisResultJSON)
	}
	if eval.Warning {
		analysis.Status.Warning = true
	}
	analysis.Status.Pass = eval.Pass
	go a.reportResultsAsPromMetric(eval, analysis)
	return a.updateStatus(ctx, analysis)
}

func (a *AnalysisReconciler) reportResultsAsPromMetric(eval evalType.AnalysisResult, analysis *metricsapi.Analysis) {
	f := analysis.Spec.From.String()
	t := analysis.Spec.To.String()
	labelsAnalysis := prometheus.Labels{
		"name":      analysis.Name,
		"namespace": analysis.Namespace,
		"from":      f,
		"to":        t,
	}
	if m, err := a.Metrics.AnalysisResult.GetMetricWith(labelsAnalysis); err == nil {
		m.Set(eval.GetAchievedPercentage())
	} else {
		a.Log.Error(err, "unable to set value for analysis result metric")
	}
	// expose also the individual objectives
	for _, o := range eval.ObjectiveResults {
		name := o.Objective.AnalysisValueTemplateRef.Name
		ns := o.Objective.AnalysisValueTemplateRef.Namespace
		labelsObjective := prometheus.Labels{
			"name":               name,
			"namespace":          ns,
			"analysis_name":      analysis.Name,
			"analysis_namespace": analysis.Namespace,
			"key_objective":      fmt.Sprintf("%v", o.Objective.KeyObjective),
			"weight":             fmt.Sprintf("%v", o.Objective.Weight),
			"from":               f,
			"to":                 t,
		}
		if m, err := a.Metrics.ObjectiveResult.GetMetricWith(labelsObjective); err == nil {
			m.Set(o.Value)
		} else {
			a.Log.Error(err, "unable to set value for objective result metric")
		}
	}
}

//nolint:ineffassign,staticcheck
func SetupMetric() (m Metrics, err error) {

	labelNamesAnalysis := []string{"name", "namespace", "from", "to"}
	a := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "keptn_analysis_result",
		Help: "Result of Analysis",
	}, labelNamesAnalysis)
	err = prometheus.Register(a)

	labelNames := []string{"name", "namespace", "analysis_name", "analysis_namespace", "key_objective", "weight", "from", "to"}
	o := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "keptn_objective_result",
		Help: "Result of the Analysis Objective",
	}, labelNames)
	err = prometheus.Register(o)

	return Metrics{
		AnalysisResult:  a,
		ObjectiveResult: o,
	}, err
}

func (a *AnalysisReconciler) updateStatus(ctx context.Context, analysis *metricsapi.Analysis) error {
	if err := a.Client.Status().Update(ctx, analysis); err != nil {
		a.Log.Error(err, "Failed to update the Analysis status")
		return err
	}
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (a *AnalysisReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&metricsapi.Analysis{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Complete(a)
}

func extractMissingObjectives(objectives []metricsapi.Objective, status map[string]metricsapi.ProviderResult) ([]metricsapi.Objective, map[string]metricsapi.ProviderResult) {
	var todo []metricsapi.Objective
	done := make(map[string]metricsapi.ProviderResult, len(status))
	for _, obj := range objectives {
		key := common.ComputeKey(obj.AnalysisValueTemplateRef)
		if value, ok := status[key]; ok {
			if value.ErrMsg != "" {
				todo = append(todo, obj)
			} else {
				done[key] = status[key]
			}
		} else {
			todo = append(todo, obj)
		}
	}
	return todo, done
}
