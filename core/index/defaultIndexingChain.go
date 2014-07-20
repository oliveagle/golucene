package index

import (
	"fmt"
	. "github.com/balzaczyy/golucene/core/codec/spi"
	. "github.com/balzaczyy/golucene/core/index/model"
	"github.com/balzaczyy/golucene/core/store"
	"github.com/balzaczyy/golucene/core/util"
)

/* Default general purpose indexing chain, which handles indexing all types of fields */
type DefaultIndexingChain struct {
	bytesUsed  util.Counter
	docState   *docState
	docWriter  *DocumentsWriterPerThread
	fieldInfos *FieldInfosBuilder

	// Writes postings and term vectors:
	termsHash TermsHash

	storedFieldsWriter StoredFieldsWriter // lazy init
	lastStoredDocId    int

	fieldHash []*PerField
	hashMask  int

	totalFieldCount int
	nextFieldGen    int64

	// Holds fields seen in each document
	fields []*PerField
}

func newDefaultIndexingChain(docWriter *DocumentsWriterPerThread) *DefaultIndexingChain {
	termVectorsWriter := newTermVectorsConsumer(docWriter)
	return &DefaultIndexingChain{
		docWriter:  docWriter,
		fieldInfos: docWriter.fieldInfos,
		docState:   docWriter.docState,
		bytesUsed:  docWriter._bytesUsed,
		termsHash:  newFreqProxTermsWriter(docWriter, termVectorsWriter),
		fieldHash:  make([]*PerField, 2),
		hashMask:   1,
		fields:     make([]*PerField, 1),
	}
}

// TODO: can we remove this lazy-init / make cleaner / do it another way...?
func (c *DefaultIndexingChain) initStoredFieldsWriter() (err error) {
	if c.storedFieldsWriter == nil {
		assert(c != nil)
		assert(c.docWriter != nil)
		assert(c.docWriter.codec != nil)
		assert(c.docWriter.codec.StoredFieldsFormat() != nil)
		c.storedFieldsWriter, err = c.docWriter.codec.StoredFieldsFormat().FieldsWriter(
			c.docWriter.directory, c.docWriter.segmentInfo, store.IO_CONTEXT_DEFAULT)
	}
	return
}

func (c *DefaultIndexingChain) flush(state *SegmentWriteState) error {
	panic("not implemented yet")
}

/*
Catch up for all docs before us that had no stored fields, or hit
non-aborting errors before writing stored fields.
*/
func (c *DefaultIndexingChain) fillStoredFields(docId int) (err error) {
	for err == nil && c.lastStoredDocId < docId {
		err = c.startStoredFields()
		if err == nil {
			err = c.finishStoredFields()
		}
	}
	return
}

func (c *DefaultIndexingChain) abort() {
	// E.g. close any open files in the stored fields writer:
	if c.storedFieldsWriter != nil {
		c.storedFieldsWriter.Abort() // ignore error
	}

	// E.g. close any open files in the term vectors writer:
	c.termsHash.abort()

	for i, _ := range c.fieldHash {
		c.fieldHash[i] = nil
	}
}

func (c *DefaultIndexingChain) rehash() {
	newHashSize := 2 * len(c.fieldHash)
	assert(newHashSize > len(c.fieldHash))

	newHashArray := make([]*PerField, newHashSize)

	// rehash
	newHashMask := newHashSize - 1
	for _, fp0 := range c.fieldHash {
		for fp0 != nil {
			hashPos2 := hashstr(fp0.fieldInfo.Name) & newHashMask
			fp0.next, newHashArray[hashPos2], fp0 =
				newHashArray[hashPos2], fp0, fp0.next
		}
	}

	c.fieldHash = newHashArray
	c.hashMask = newHashMask
}

/* Calls StoredFieldsWriter.startDocument, aborting the segment if it hits any error. */
func (c *DefaultIndexingChain) startStoredFields() (err error) {
	var success = false
	defer func() {
		if !success {
			c.docWriter.setAborting()
		}
	}()

	if err = c.initStoredFieldsWriter(); err != nil {
		return
	}
	if err = c.storedFieldsWriter.StartDocument(); err != nil {
		return
	}
	success = true

	c.lastStoredDocId++
	return nil
}

/* Calls StoredFieldsWriter.finishDocument(), aborting the segment if it hits any error. */
func (c *DefaultIndexingChain) finishStoredFields() error {
	var success = false
	defer func() {
		if !success {
			c.docWriter.setAborting()
		}
	}()
	if err := c.storedFieldsWriter.FinishDocument(); err != nil {
		return err
	}
	success = true
	return nil
}

func (c *DefaultIndexingChain) processDocument() (err error) {
	// How many indexed field names we've seen (collapses multiple
	// field instances by the same name):
	fieldCount := 0

	fieldGen := c.nextFieldGen
	c.nextFieldGen++

	// NOTE: we need to passes here, in case there are multi-valued
	// fields, because we must process all instances of a given field
	// at once, since the anlayzer is free to reuse TOkenStream across
	// fields (i.e., we cannot have more than one TokenStream running
	// "at once"):

	c.termsHash.startDocument()

	if err = c.fillStoredFields(c.docState.docID); err != nil {
		return
	}
	if err = c.startStoredFields(); err != nil {
		return
	}

	if err = func() error {
		defer func() {
			if !c.docWriter.aborting {
				// Finish each indexed field name seen in the document:
				for _, field := range c.fields[:fieldCount] {
					err = mergeError(err, field.finish())
				}
				err = mergeError(err, c.finishStoredFields())
			}
		}()

		for _, field := range c.docState.doc {
			if fieldCount, err = c.processField(field, fieldGen, fieldCount); err != nil {
				return err
			}
		}
		return nil
	}(); err != nil {
		return
	}

	var success = false
	defer func() {
		if !success {
			// Must abort, on the possibility that on-disk term vectors are now corrupt:
			c.docWriter.setAborting()
		}
	}()

	if err = c.termsHash.finishDocument(); err != nil {
		return
	}
	success = true
	return nil
}

func (c *DefaultIndexingChain) processField(field IndexableField,
	fieldGen int64, fieldCount int) (int, error) {

	var fieldName string = field.Name()
	var fieldType IndexableFieldType = field.FieldType()
	var fp *PerField

	// Invert indexed fields:
	if fieldType.Indexed() {

		// if the field omits norms, the boost cannot be indexed.
		if fieldType.OmitNorms() && field.Boost() != 1 {
			panic(fmt.Sprintf(
				"You cannot set an index-time boost: norms are omitted for field '%v'",
				fieldName))
		}

		fp = c.getOrAddField(fieldName, fieldType, true)
		first := fp.fieldGen != fieldGen
		if err := fp.invert(field, first); err != nil {
			return 0, err
		}

		if first {
			c.fields[fieldCount] = fp
			fieldCount++
			fp.fieldGen = fieldGen
		}
	} else {
		panic("not implemented yet")
	}

	// Add stored fields:
	if fieldType.Stored() {
		panic("not implemented yet")
	} else {
		panic("not implemented yet")
	}

	if dvType := fieldType.DocValueType(); int(dvType) != 0 {
		if fp == nil {
			panic("not implemented yet")
		}
		panic("not implemented yet")
	}

	return fieldCount, nil
}

func (c *DefaultIndexingChain) getOrAddField(name string,
	fieldType IndexableFieldType, invert bool) *PerField {

	// Make sure we have a PerField allocated
	hashPos := hashstr(name) & c.hashMask
	fp := c.fieldHash[hashPos]
	for fp != nil && fp.fieldInfo.Name != name {
		fp = fp.next
	}

	if fp == nil {
		// First time we are seeing this field in this segment

		fi := c.fieldInfos.AddOrUpdate(name, fieldType)

		fp = newPerField(c, fi, invert)
		fp.next = c.fieldHash[hashPos]
		c.fieldHash[hashPos] = fp
		c.totalFieldCount++

		// At most 50% load factor:
		if c.totalFieldCount >= len(c.fieldHash)/2 {
			c.rehash()
		}

		if c.totalFieldCount > len(c.fieldHash) {
			newFields := make([]*PerField, util.Oversize(c.totalFieldCount, util.NUM_BYTES_OBJECT_REF))
			copy(newFields, c.fields)
			c.fields = newFields
		}

	} else {
		panic("not implemented yet")
	}

	return fp
}

const primeRK = 16777619

/* simple string hash used by Go strings package */
func hashstr(sep string) int {
	hash := uint32(0)
	for i := 0; i < len(sep); i++ {
		hash = hash*primeRK + uint32(sep[i])
	}
	return int(hash)
}

type PerField struct {
	*DefaultIndexingChain // acess at least docState, termsHash.

	fieldInfo  *FieldInfo
	similarity Similarity

	invertState       *FieldInvertState
	termsHashPerField TermsHashPerField

	// We use this to know when a PerField is seen for the first time
	// in the current document.
	fieldGen int64

	// Used by the hash table
	next *PerField
}

func newPerField(parent *DefaultIndexingChain,
	fieldInfo *FieldInfo, invert bool) *PerField {

	ans := &PerField{
		DefaultIndexingChain: parent,
		fieldInfo:            fieldInfo,
		similarity:           parent.docState.similarity,
		fieldGen:             -1,
	}
	if invert {
		ans.setInvertState()
	}
	return ans
}

func (f *PerField) setInvertState() {
	f.invertState = newFieldInvertState(f.fieldInfo.Name)
	f.termsHashPerField = f.termsHash.addField(f.invertState, f.fieldInfo)
}

func (f *PerField) finish() error {
	panic("not implemented yet")
}

/*
Inverts one field for one document; first is true if this is the
first time we are seeing this field name in this document.
*/
func (f *PerField) invert(field IndexableField, first bool) error {
	panic("not implemented yet")
}
