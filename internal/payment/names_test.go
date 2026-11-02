package payment_test

import (
	"reflect"
	"runtime"
	"strings"
)

func activityName(activity any) string {
	name := runtime.FuncForPC(reflect.ValueOf(activity).Pointer()).Name()
	name = strings.TrimSuffix(name, "-fm")
	return name[strings.LastIndex(name, ".")+1:]
}
