package provider

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/types"

	"github.com/gorilla/mux"
)

func DashboardRouter(r *mux.Router, db *sql.DB) {
	r.Path("/nodes/{Name}/metrics/{MetricName}/{Whatever}").HandlerFunc(nodeHandler(db))
	r.Path("/namespaces/{Namespace}/pod-list/{Name}/metrics/{MetricName}/{Whatever}").HandlerFunc(podHandler(db))
	r.PathPrefix("/").HandlerFunc(defaultHandler)
}

func defaultHandler(w http.ResponseWriter, r *http.Request) {
	msg := fmt.Sprintf("%v - URL: %s", time.Now(), r.URL)
	w.Write([]byte(msg))
}

func nodeHandler(db *sql.DB) http.HandlerFunc {
	fn := func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)

		resp, err := getNodeMetrics(db, vars["MetricName"], ResourceSelector{
			Namespace:    "",
			ResourceName: vars["Name"],
		})

		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(fmt.Sprintf("Node Metrics Error - %v", err.Error())))
		}

		j, err := json.Marshal(resp)

		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(fmt.Sprintf("JSON Error - %v", err.Error())))
		}

		w.Write([]byte(j))
	}

	return http.HandlerFunc(fn)
}

func podHandler(db *sql.DB) http.HandlerFunc {
	fn := func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)

		resp, err := getPodMetrics(db, vars["MetricName"], ResourceSelector{
			Namespace:    vars["Namespace"],
			ResourceName: vars["Name"],
		})

		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(fmt.Sprintf("Pod Metrics Error - %v", err.Error())))
		}

		j, err := json.Marshal(resp)

		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(fmt.Sprintf("JSON Error - %v", err.Error())))
		}

		w.Write([]byte(j))
	}

	return http.HandlerFunc(fn)
}

func getRows(db *sql.DB, table string, metricName string, selector ResourceSelector) (*sql.Rows, error) {
	var query string
	var values []interface{}
	var args []string
	orderBy := []string{"name", "time"}
	if metricName == "cpu" {
		query = "select sum(cpu), name, uid, time from %s "
	} else {
		//default to metricName == "memory/usage"
		metricName = "memory"
		query = "select sum(memory), name, uid, time from %s "
	}

	if table == "pods" {
		orderBy = []string{"namespace", "name", "time"}
		args = append(args, "namespace=?")
		if selector.Namespace != "" {
			values = append(values, selector.Namespace)
		} else {
			values = append(values, "default")
		}
	}

	if selector.ResourceName != "" {
		if strings.ContainsAny(selector.ResourceName, ",") {
			subargs := []string{}
			for _, v := range strings.Split(selector.ResourceName, ",") {
				subargs = append(subargs, "?")
				values = append(values, v)
			}
			args = append(args, " name in ("+strings.Join(subargs, ",")+")")
		} else {
			values = append(values, selector.ResourceName)
			args = append(args, " name = ?")
		}
	}
	if selector.UID != "" {
		args = append(args, " uid = ?")
		values = append(values, selector.UID)
	}

	query = fmt.Sprintf(query+" where "+strings.Join(args, " and ")+" group by name, time order by %v;", table, strings.Join(orderBy, ", "))

	log.Infof("Query: %s", query)
	log.Infof("Values: %v", values)

	return db.Query(query, values...)
}

/*
	getPodMetrics: With a database connection and a resource selector
	Queries SQLite and returns a list of metrics.
*/
func getPodMetrics(db *sql.DB, metricName string, selector ResourceSelector) (SidecarMetricResultList, error) {
	rows, err := getRows(db, "pods", metricName, selector)
	if err != nil {
		log.Errorf("Error getting pod metrics: %v", err)
		return SidecarMetricResultList{}, err
	}

	defer rows.Close()

	resultList := make(map[string]SidecarMetric)

	for rows.Next() {
		var metricValue string
		var pod string
		var metricTime string
		var uid string
		var newMetric MetricPoint
		err = rows.Scan(&metricValue, &pod, &uid, &metricTime)
		if err != nil {
			return SidecarMetricResultList{}, err
		}

		layout := "2006-01-02T15:04:05Z"
		t, err := time.Parse(layout, metricTime)
		if err != nil {
			return SidecarMetricResultList{}, err
		}

		v, err := strconv.ParseUint(metricValue, 10, 64)

		if metricName == "memory" {
			newMetric = MetricPoint{
				Timestamp: t,
				Value:     v / 1000,
			}
		} else {
			newMetric = MetricPoint{
				Timestamp: t,
				Value:     v,
			}
		}

		if _, ok := resultList[pod]; ok {
			metricThing := resultList[pod]
			metricThing.AddMetricPoint(newMetric)
			resultList[pod] = metricThing
		} else {
			resultList[pod] = SidecarMetric{
				MetricName:   metricName,
				MetricPoints: []MetricPoint{newMetric},
				DataPoints:   []DataPoint{},
				UIDs: []types.UID{
					types.UID(pod),
				},
			}
		}
	}
	err = rows.Err()
	if err != nil {
		return SidecarMetricResultList{}, err
	}

	result := SidecarMetricResultList{}
	for _, v := range resultList {
		result.Items = append(result.Items, v)
	}

	return result, nil
}

/*
	getNodeMetrics: With a database connection and a resource selector
	Queries SQLite and returns a list of metrics.
*/
func getNodeMetrics(db *sql.DB, metricName string, selector ResourceSelector) (SidecarMetricResultList, error) {
	stripNum := 2
	if metricName == "cpu" {
		stripNum = 1
	}

	resultList := make(map[string]SidecarMetric)

	rows, err := getRows(db, "nodes", metricName, selector)

	if err != nil {
		log.Errorf("Error getting node metrics: %v", err)
		return SidecarMetricResultList{}, err
	}

	defer rows.Close()
	for rows.Next() {
		var metricValue string
		var node string
		var metricTime string
		var uid string
		var newMetric MetricPoint
		err = rows.Scan(&metricValue, &node, &uid, &metricTime)
		if err != nil {
			return SidecarMetricResultList{}, err
		}

		layout := "2006-01-02T15:04:05Z"
		t, err := time.Parse(layout, metricTime)
		if err != nil {
			return SidecarMetricResultList{}, err
		}

		v, err := strconv.ParseUint(metricValue[0:len(metricValue)-stripNum], 10, 64)

		if metricName == "memory" {
			newMetric = MetricPoint{
				Timestamp: t,
				Value:     v / 10,
			}
		} else {
			newMetric = MetricPoint{
				Timestamp: t,
				Value:     v,
			}
		}

		if _, ok := resultList[node]; ok {
			metricThing := resultList[node]
			metricThing.AddMetricPoint(newMetric)
			resultList[node] = metricThing
		} else {
			resultList[node] = SidecarMetric{
				MetricName:   metricName,
				MetricPoints: []MetricPoint{newMetric},
				DataPoints:   []DataPoint{},
				UIDs: []types.UID{
					types.UID(node),
				},
			}
		}
	}
	err = rows.Err()
	if err != nil {
		return SidecarMetricResultList{}, err
	}

	result := SidecarMetricResultList{}
	for _, v := range resultList {
		result.Items = append(result.Items, v)
	}

	return result, nil
}
