package index

type DocFieldConsumerPerField interface {
	abort()
	fieldInfo() FieldInfo
}