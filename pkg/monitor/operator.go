package monitor

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/tools/cache"

	configv1 "github.com/openshift/api/config/v1"
	configclientset "github.com/openshift/client-go/config/clientset/versioned"
)

// condition/Degraded status/True reason/DNSDegraded changed: DNS default is degraded
func GetOperatorConditionStatus(message string) (string, bool, string) {
	if !strings.HasPrefix(message, "condition/") {
		return "", false, ""
	}
	stanzas := strings.Split(message, " ")
	if len(stanzas) < 2 {
		return "", false, ""
	}

	conditions := strings.Split(stanzas[0], "/")
	if len(conditions) != 2 {
		return "", false, ""
	}
	statusStrings := strings.Split(stanzas[1], "/")
	if len(statusStrings) != 2 {
		return "", false, ""
	}
	status, err := strconv.ParseBool(statusStrings[1])
	if err != nil {
		return "", false, ""
	}

	messages := strings.SplitN(message, ": ", 2)
	if len(messages) < 2 {
		return conditions[1], status, ""
	}

	return conditions[1], status, messages[1]
}

func startClusterOperatorMonitoring(ctx context.Context, m Recorder, client configclientset.Interface) {
	coInformer := cache.NewSharedIndexInformer(
		NewErrorRecordingListWatcher(m, &cache.ListWatch{
			ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
				return client.ConfigV1().ClusterOperators().List(ctx, options)
			},
			WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
				return client.ConfigV1().ClusterOperators().Watch(ctx, options)
			},
		}),
		&configv1.ClusterOperator{},
		time.Hour,
		nil,
	)

	coChangeFns := []func(co, oldCO *configv1.ClusterOperator) []Condition{
		func(co, oldCO *configv1.ClusterOperator) []Condition {
			var conditions []Condition
			for i := range co.Status.Conditions {
				c := &co.Status.Conditions[i]
				previous := findOperatorStatusCondition(oldCO.Status.Conditions, c.Type)
				if previous == nil {
					continue
				}
				if c.Status != previous.Status {
					var msg string
					switch {
					case len(c.Reason) > 0 && len(c.Message) > 0:
						msg = fmt.Sprintf("condition/%s status/%s reason/%s changed: %s", c.Type, c.Status, c.Reason, c.Message)
					case len(c.Message) > 0:
						msg = fmt.Sprintf("condition/%s status/%s changed: %s", c.Type, c.Status, c.Message)
					default:
						msg = fmt.Sprintf("condition/%s status/%s changed: ", c.Type, c.Status)
					}
					level := Warning
					if c.Type == configv1.OperatorDegraded && c.Status == configv1.ConditionTrue {
						level = Error
					}
					if c.Type == configv1.OperatorAvailable && c.Status == configv1.ConditionFalse {
						level = Error
					}
					if c.Type == configv1.OperatorProgressing && c.Status == configv1.ConditionTrue {
						level = Warning
					}
					if c.Type == configv1.ClusterStatusConditionType("Failing") && c.Status == configv1.ConditionTrue {
						level = Error
					}
					conditions = append(conditions, Condition{
						Level:   level,
						Locator: locateClusterOperator(co),
						Message: msg,
					})
				}
			}
			if changes := findOperatorVersionChange(oldCO.Status.Versions, co.Status.Versions); len(changes) > 0 {
				conditions = append(conditions, Condition{
					Level:   Info,
					Locator: locateClusterOperator(co),
					Message: fmt.Sprintf("versions: %v", strings.Join(changes, ", ")),
				})
			}
			return conditions
		},
	}

	startTime := time.Now().Add(-time.Minute)
	coInformer.AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				co, ok := obj.(*configv1.ClusterOperator)
				if !ok {
					return
				}
				// filter out old pods so our monitor doesn't send a big chunk
				// of co creations
				if co.CreationTimestamp.Time.Before(startTime) {
					return
				}
				m.Record(Condition{
					Level:   Info,
					Locator: locateClusterOperator(co),
					Message: "created",
				})
			},
			DeleteFunc: func(obj interface{}) {
				co, ok := obj.(*configv1.ClusterOperator)
				if !ok {
					return
				}
				m.Record(Condition{
					Level:   Warning,
					Locator: locateClusterOperator(co),
					Message: "deleted",
				})
			},
			UpdateFunc: func(old, obj interface{}) {
				co, ok := obj.(*configv1.ClusterOperator)
				if !ok {
					return
				}
				oldCO, ok := old.(*configv1.ClusterOperator)
				if !ok {
					return
				}
				if co.UID != oldCO.UID {
					return
				}
				for _, fn := range coChangeFns {
					m.Record(fn(co, oldCO)...)
				}
			},
		},
	)

	go coInformer.Run(ctx.Done())

	cvInformer := cache.NewSharedIndexInformer(
		NewErrorRecordingListWatcher(m, &cache.ListWatch{
			ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
				options.FieldSelector = "metadata.name=version"
				return client.ConfigV1().ClusterVersions().List(ctx, options)
			},
			WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
				options.FieldSelector = "metadata.name=version"
				return client.ConfigV1().ClusterVersions().Watch(ctx, options)
			},
		}),
		&configv1.ClusterVersion{},
		time.Hour,
		nil,
	)

	cvChangeFns := []func(cv, oldCV *configv1.ClusterVersion) []Condition{
		func(cv, oldCV *configv1.ClusterVersion) []Condition {
			var conditions []Condition
			if len(cv.Status.History) == 0 {
				return nil
			}
			if len(oldCV.Status.History) == 0 {
				conditions = append(conditions, Condition{
					Level:   Warning,
					Locator: locateClusterVersion(cv),
					Message: fmt.Sprintf("cluster converging to %s", versionOrImage(cv.Status.History[0])),
				})
				return conditions
			}
			cvNew, cvOld := cv.Status.History[0], oldCV.Status.History[0]
			switch {
			case cvNew.State == configv1.CompletedUpdate && cvOld.State != cvNew.State:
				conditions = append(conditions, Condition{
					Level:   Warning,
					Locator: locateClusterVersion(cv),
					Message: fmt.Sprintf("cluster reached %s", versionOrImage(cvNew)),
				})
			case cvNew.State == configv1.PartialUpdate && cvOld.State == cvNew.State && cvOld.Image != cvNew.Image:
				conditions = append(conditions, Condition{
					Level:   Warning,
					Locator: locateClusterVersion(cv),
					Message: fmt.Sprintf("cluster upgrading to %s without completing %s", versionOrImage(cvNew), versionOrImage(cvOld)),
				})
			}
			return conditions
		},
		func(cv, oldCV *configv1.ClusterVersion) []Condition {
			var conditions []Condition
			for i := range cv.Status.Conditions {
				s := &cv.Status.Conditions[i]
				previous := findOperatorStatusCondition(oldCV.Status.Conditions, s.Type)
				if previous == nil {
					continue
				}
				if s.Status != previous.Status {
					var msg string
					switch {
					case len(s.Reason) > 0 && len(s.Message) > 0:
						msg = fmt.Sprintf("changed %s to %s: %s: %s", s.Type, s.Status, s.Reason, s.Message)
					case len(s.Message) > 0:
						msg = fmt.Sprintf("changed %s to %s: %s", s.Type, s.Status, s.Message)
					default:
						msg = fmt.Sprintf("changed %s to %s", s.Type, s.Status)
					}
					level := Warning
					if s.Type == configv1.OperatorDegraded && s.Status == configv1.ConditionTrue {
						level = Error
					}
					if s.Type == configv1.ClusterStatusConditionType("Failing") && s.Status == configv1.ConditionTrue {
						level = Error
					}
					conditions = append(conditions, Condition{
						Level:   level,
						Locator: locateClusterVersion(cv),
						Message: msg,
					})
				}
			}
			return conditions
		},
	}

	cvInformer.AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				cv, ok := obj.(*configv1.ClusterVersion)
				if !ok {
					return
				}
				// filter out old pods so our monitor doesn't send a big chunk
				// of co creations
				if cv.CreationTimestamp.Time.Before(startTime) {
					return
				}
				m.Record(Condition{
					Level:   Info,
					Locator: locateClusterVersion(cv),
					Message: "created",
				})
			},
			DeleteFunc: func(obj interface{}) {
				cv, ok := obj.(*configv1.ClusterVersion)
				if !ok {
					return
				}
				m.Record(Condition{
					Level:   Warning,
					Locator: locateClusterVersion(cv),
					Message: "deleted",
				})
			},
			UpdateFunc: func(old, obj interface{}) {
				cv, ok := obj.(*configv1.ClusterVersion)
				if !ok {
					return
				}
				oldCV, ok := old.(*configv1.ClusterVersion)
				if !ok {
					return
				}
				if cv.UID != oldCV.UID {
					return
				}
				for _, fn := range cvChangeFns {
					m.Record(fn(cv, oldCV)...)
				}
			},
		},
	)

	m.AddSampler(func(now time.Time) []*Condition {
		var conditions []*Condition
		for _, obj := range cvInformer.GetStore().List() {
			cv, ok := obj.(*configv1.ClusterVersion)
			if !ok {
				continue
			}
			if len(cv.Status.History) > 0 {
				if cv.Status.History[0].State != configv1.CompletedUpdate {
					conditions = append(conditions, &Condition{
						Level:   Warning,
						Locator: locateClusterVersion(cv),
						Message: fmt.Sprintf("cluster is updating to %s", versionOrImage(cv.Status.History[0])),
					})
				}
			}
		}
		return conditions
	})

	go cvInformer.Run(ctx.Done())
}

func versionOrImage(h configv1.UpdateHistory) string {
	if len(h.Version) == 0 {
		return h.Image
	}
	return h.Version
}

func locateClusterOperator(co *configv1.ClusterOperator) string {
	return fmt.Sprintf("clusteroperator/%s", co.Name)
}

func locateClusterVersion(cv *configv1.ClusterVersion) string {
	return fmt.Sprintf("clusterversion/%s", cv.Name)
}

func findOperatorVersionChange(old, new []configv1.OperandVersion) []string {
	var changed []string
	for i := 0; i < len(new); i++ {
		for j := 0; j < len(old); j++ {
			p := (j + i) % len(old)
			if old[p].Name != new[i].Name {
				continue
			}
			if old[p].Version == new[i].Version {
				break
			}
			changed = append(changed, fmt.Sprintf("%s %s -> %s", new[i].Name, old[p].Version, new[i].Version))
			break
		}
	}
	return changed
}

func findOperatorStatusCondition(conditions []configv1.ClusterOperatorStatusCondition, conditionType configv1.ClusterStatusConditionType) *configv1.ClusterOperatorStatusCondition {
	for i := range conditions {
		if conditions[i].Type == conditionType {
			return &conditions[i]
		}
	}
	return nil
}
