package socket

import "log"

type binJsonValue interface {
	replacePlaceholder(num int, value interface{}) bool
}

type binJsonArray []binJsonValue
type binJsonObject map[string]binJsonValue
type binJsonPrimitive struct {
	value       interface{}
	placeholder int // num of placeholder
}

type binJsonNull struct{}

func (n *binJsonNull) replacePlaceholder(num int, value interface{}) bool { return false }

func (a binJsonArray) replacePlaceholder(num int, value interface{}) bool {
	for _, v := range a {
		if v.replacePlaceholder(num, value) {
			return true
		}
	}
	return false
}

func (m binJsonObject) replacePlaceholder(num int, value interface{}) bool {
	for _, v := range m {
		if v.replacePlaceholder(num, value) {
			return true
		}
	}
	return false
}

func (p *binJsonPrimitive) replacePlaceholder(num int, value interface{}) bool {
	if p.value != nil {
		return false
	}
	if p.placeholder != num {
		return false
	}
	p.value = value
	return true
}

type binJsonConverter struct {
	placeholders map[int]binJsonValue
}

func (cv *binJsonConverter) tryConvertToPlaceholder(b interface{}) (binJsonValue, bool) {
	placeholderObj, ok := b.(map[string]interface{})
	if !ok {
		return nil, false
	}

	_, ok = placeholderObj["_placeholder"]
	if !ok {
		return nil, false
	}

	num, ok := placeholderObj["num"]
	if !ok {
		return nil, false
	}

	numInt, ok := num.(int)
	if !ok {
		return nil, false
	}

	val := &binJsonPrimitive{placeholder: numInt}
	cv.placeholders[numInt] = val

	return val, true
}

func (cv *binJsonConverter) convertToBinJson(b interface{}) binJsonValue {
	placeholder, ok := cv.tryConvertToPlaceholder(b)
	if ok {
		return placeholder
	}

	switch v := b.(type) {
	case []interface{}:
		a := make(binJsonArray, len(v))
		for i, vv := range v {
			a[i] = cv.convertToBinJson(vv)
		}
		return a
	case map[string]interface{}:
		m := make(binJsonObject)
		for k, vv := range v {
			m[k] = cv.convertToBinJson(vv)
		}
		return m
	case nil:
		return &binJsonNull{}
	default:
		return &binJsonPrimitive{value: v}
	}
}

func convertToBinJson(b interface{}) (binJsonValue, map[int]binJsonValue) {
	cv := binJsonConverter{placeholders: make(map[int]binJsonValue)}
	return cv.convertToBinJson(b), cv.placeholders
}

func convertToJson(b binJsonValue) interface{} {
	switch v := b.(type) {
	case binJsonArray:
		a := make([]interface{}, len(v))
		for i, vv := range v {
			a[i] = convertToJson(vv)
		}
		return a
	case binJsonObject:
		m := make(map[string]interface{})
		for k, vv := range v {
			m[k] = convertToJson(vv)
		}
		return m
	case *binJsonPrimitive:
		return v.value
	case *binJsonNull:
		return nil
	}
	log.Printf("unreachable")
	return nil
}
