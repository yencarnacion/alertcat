// File: internal/alerts/alerts.go
package alerts

import (
	"encoding/csv"
	"fmt"
	"os"
	"time"
)

type Alert struct {
	Timestamp time.Time
	Symbol    string
	Price     float64
	Volume    int64
	Baseline  float64
	RVOL      float64
	Method    string
	Bucket    string
	Threshold float64
}

// LogToCSV appends a single alert row into alerts_YYYYMMDD.csv
func LogToCSV(alert Alert) error {
	filename := fmt.Sprintf("alerts_%s.csv", alert.Timestamp.Format("20060102"))
	f, err := os.OpenFile(filename, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()

	w := csv.NewWriter(f)
	defer w.Flush()

	row := []string{
		alert.Timestamp.Format(time.RFC3339),
		alert.Symbol,
		fmt.Sprintf("%.4f", alert.Price),
		fmt.Sprintf("%d", alert.Volume),
		fmt.Sprintf("%.0f", alert.Baseline),
		fmt.Sprintf("%.2f", alert.RVOL),
		alert.Method,
		alert.Bucket,
		fmt.Sprintf("%.2f", alert.Threshold),
	}
	return w.Write(row)
}
