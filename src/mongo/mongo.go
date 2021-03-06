//    Copyright (C) 2016  mparaiso <mparaiso@online.fr>
//
//    Licensed under the Apache License, Version 2.0 (the "License");
//    you may not use this file except in compliance with the License.
//    You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
//    Unless required by applicable law or agreed to in writing, software
//    distributed under the License is distributed on an "AS IS" BASIS,
//    WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//    See the License for the specific language governing permissions and
//    limitations under the License.

// Package mongo provides an object document mapper, or ODM for mongodb, strongly influenced by Doctrine Mongo ODM.
package mongo

import (
	"fmt"
	"reflect"
	"strings"

	"../funcs"
	"../logger"
	"../tag"

	"gopkg.in/mgo.v2"
	"gopkg.in/mgo.v2/bson"
)

var (
	// ErrDocumentNotRegistered is yielded when a type that has not been registered is requested by the DocumentManager
	ErrDocumentNotRegistered = fmt.Errorf("Error the type of the document was not registered in the document manager")
	// ErrIDFieldNotFound is yielded when _id field wasn't found in a struct
	ErrIDFieldNotFound = fmt.Errorf("Error no _id field defined for type")
	// ErrMappedFieldNotFound is yielded when the field of a mappedBy annotation was not found
	ErrMappedFieldNotFound = fmt.Errorf("Error mapped field not found, check mappedBy annotation for document")
	// ErrNotAstruct is yielded when a struct was expected
	ErrNotAstruct = fmt.Errorf("Error a struct was expected")
	// ErrNotAPointer is yielded when a pointer was expected
	ErrNotAPointer = fmt.Errorf("Error a pointer was expected")
	// ErrNotAnArray is yielded when an array was expected
	ErrNotAnArray = fmt.Errorf("Error an array was expected")
	// ErrNotImpletemented is yielded when a method was called yet is not implemented
	ErrNotImpletemented = fmt.Errorf("Error a called method is not implemented")
	// ErrFieldNotFound : Error a field metada was requested and not found
	ErrFieldNotFound = fmt.Errorf("Error a field metada was requested and not found ")
	// ErrInvalidAnnotation : An invalid mongo-odm annotation was found , check your odm struct tag
	ErrInvalidAnnotation = fmt.Errorf("An invalid mongo-odm annotation was found , check your odm struct tag")
	zeroMetadata         = metadata{}
	zeroRelation         = relation{}
	// ZeroObjectID represents a zero value for bson.ObjectId
	zeroObjectID = reflect.Zero(reflect.TypeOf(bson.NewObjectId())).Interface().(bson.ObjectId)
)

// DocumentManager is a mongodb document manager
type DocumentManager interface {

	// Register a new document type, targetDocument is the name of the document and the collection name,
	// document is a pointer to struct.
	// returns an error on error.
	// use DocumentManager.RegisterMany to register many documents at the same time.
	Register(collectionName string, value interface{}) error

	// register many documents or returns an error on error
	RegisterMany(documents map[string]interface{}) error

	// Persist saves a document. No document is sent to the db
	// until flush is called
	Persist(document interface{})

	// Remove deletes a document. Flush must be called to commit changes
	// to the database
	Remove(document interface{})

	// Flush executes saves,updates and removes pending in the document manager
	Flush() error

	// FindID finds a document by ID
	FindID(id interface{}, returnValue interface{}) error

	// FIndOne finds a single document
	FindOne(query interface{}, returnValue interface{}) error

	// FindBy find documents by query
	FindBy(query interface{}, returnValues interface{}) error

	// FIndAll find all documents in a collection
	FindAll(returnValues interface{}) error

	// GetDB returns the driver's DB
	GetDB() *mgo.Database

	// SetLogger sets the logger
	SetLogger(logger.Logger)

	// CreateQuery creates a query builder for complex queries
	CreateQuery() queryBuilder
}

// TODO DocumentManager.ResolveRelations resolve relationships for a document or a collection
// of documents, or returns an error
// given a struct T, it expects either *T or *[]*T
// ResolveRelations(documentOrCollection interface{})error

type defaultDocumentManager struct {
	database  *mgo.Database
	metadatas metadatas
	tasks     tasks
	logger    logger.Logger
}

// NewDocumentManager returns a DocumentManager
func NewDocumentManager(database *mgo.Database) DocumentManager {
	return &defaultDocumentManager{database: database, metadatas: map[reflect.Type]metadata{}, tasks: tasks{}}
}

// GetDB returns the original mongodb connection
func (manager *defaultDocumentManager) GetDB() *mgo.Database {
	return manager.database
}

func (manager *defaultDocumentManager) SetLogger(Logger logger.Logger) {
	manager.logger = Logger
}

func (manager *defaultDocumentManager) log(messages ...interface{}) {
	if manager.logger != nil {
		manager.logger.Log(logger.Debug, messages...)
	}
}

// Register a new document type, targetDocument is the name of the document and the collection name,
// document is a pointer to struct.
// use DocumentManager.RegisterMany to register many documents at the same time.
func (manager *defaultDocumentManager) Register(targetDocument string, document interface{}) error {
	documentType := reflect.TypeOf(document)
	if documentType.Kind() != reflect.Ptr {
		return ErrNotAPointer
	}
	if documentType.Elem().Kind() != reflect.Struct {
		return ErrNotAstruct
	}
	meta, err := getTypeMetadatas(document)
	if err != nil {
		return err
	}
	meta.structType = documentType
	meta.targetDocument = targetDocument
	// parser := tag.NewParser(strings.NewReader(s string) )
	manager.metadatas[documentType] = meta

	manager.log("Type registered :", targetDocument, meta)
	return nil
}

func (manager *defaultDocumentManager) RegisterMany(documents map[string]interface{}) error {
	for targetDocument, document := range documents {
		if err := manager.Register(targetDocument, document); err != nil {
			return err
		}
	}
	return nil
}

func (manager *defaultDocumentManager) Persist(value interface{}) {
	if id, _ := manager.metadatas.getDocumentID(value); !id.Valid() {
		// new document, insert
		manager.metadatas.setIDForValue(value, bson.NewObjectId())
		manager.tasks[value] = insert
		return
	}
	// has an id, upsert
	manager.tasks[value] = update
}

func (manager *defaultDocumentManager) Remove(document interface{}) {
	manager.tasks[document] = del
}

func (manager *defaultDocumentManager) Flush() error {
	// TODO : a document should be flushed only once
	// keep track of a document that has already been flushed
	// and don't had it again to the tasks.
	// removing should take priority on persisting.
	for len(manager.tasks) != 0 {
		document, theTask := manager.tasks.pop()
		switch theTask {
		case del:
			if err := manager.doRemove(document); err != nil {
				return err
			}
		case insert, update:
			metaData, err := manager.metadatas.getMetadatas(reflect.TypeOf(document))
			if err != nil {
				return err
			}
			// deal with index creation
			// TODO it should be dealt with once
			if metaData.hasFieldWithIndex() {
				for _, index := range metaData.getIndexes() {
					err = manager.database.C(metaData.targetDocument).EnsureIndex(index)
					if err != nil {
						return err
					}
				}

			}

			// deal with composite index creation
			// TODO it should be dealt with once
			if metaData.hasFieldWithComposite() {
				for _, index := range metaData.getComposites() {
					err = manager.database.C(metaData.targetDocument).EnsureIndex(index)
					if err != nil {
						return err
					}
				}

			}
			if err := manager.doPersist(document); err != nil {
				return err
			}
		}
	}
	return nil
}

func (manager *defaultDocumentManager) FindBy(query interface{}, documents interface{}) error {
	Value := reflect.ValueOf(documents)
	if Value.Kind() != reflect.Ptr {
		return ErrNotAPointer
	}
	if Value.Elem().Kind() != reflect.Array && Value.Elem().Kind() != reflect.Slice {
		return ErrNotAnArray
	}
	Type := Value.Elem().Type().Elem()
	meta, ok := manager.metadatas[Type]
	if !ok {
		return ErrDocumentNotRegistered
	}
	if err := manager.database.C(meta.targetDocument).Find(query).All(documents); err != nil {
		return err
	}
	return manager.resolveRelations(documents)
}

func (manager *defaultDocumentManager) FindAll(documents interface{}) error {
	Value := reflect.ValueOf(documents)
	if Value.Kind() != reflect.Ptr {
		return ErrNotAPointer
	}
	if Value.Elem().Kind() != reflect.Array && Value.Elem().Kind() != reflect.Slice {
		return ErrNotAnArray
	}
	Type := Value.Elem().Type().Elem()
	meta, ok := manager.metadatas[Type]
	if !ok {
		return ErrDocumentNotRegistered
	}
	if err := manager.database.C(meta.targetDocument).Find(nil).All(documents); err != nil {
		return err
	}
	return manager.resolveRelations(documents)
}

func (manager *defaultDocumentManager) FindOne(query interface{}, document interface{}) error {
	Value := reflect.ValueOf(document)
	if Value.Kind() != reflect.Ptr {
		return ErrNotAPointer
	}
	if Value.Elem().Kind() != reflect.Struct {
		return ErrNotAstruct
	}
	meta, ok := manager.metadatas[reflect.TypeOf(document)]
	if !ok {
		return ErrDocumentNotRegistered
	}
	if err := manager.database.C(meta.targetDocument).Find(query).One(document); err != nil {
		return err
	}
	return manager.resolveRelations(document)
}

func (manager *defaultDocumentManager) FindID(documentID interface{}, document interface{}) error {
	Value := reflect.ValueOf(document)
	if Value.Kind() != reflect.Ptr {
		return ErrNotAPointer
	}
	if Value.Elem().Kind() != reflect.Struct {
		return ErrNotAstruct
	}
	meta, ok := manager.metadatas[reflect.TypeOf(document)]
	if !ok {
		return ErrDocumentNotRegistered
	}
	err := manager.database.C(meta.targetDocument).FindId(documentID).One(document)
	if err != nil {
		return err
	}
	return manager.resolveRelations(document)
}

func (manager *defaultDocumentManager) CreateQuery() queryBuilder {
	return newDefaultQueryBuilder(manager)
}

func (manager *defaultDocumentManager) structToMap(value interface{}) map[string]interface{} {
	// structToMap turns a struct into a map
	// ignored fields  and relations are ignored along with zero values if omitempty is configured
	result := map[string]interface{}{}
	Value := reflect.ValueOf(value)
	meta := manager.metadatas[Value.Type()]
	for _, field := range meta.fields {
		if field.ignore || (field.omitempty && isZero(Value.Elem().FieldByName(field.name).Interface())) {
			continue
		}
		if field.hasRelation() {
			continue
		}
		if field.name == meta.idField {
			result["_id"] = Value.Elem().FieldByName(field.name).Interface()
			continue
		}
		result[field.key] = Value.Elem().FieldByName(field.name).Interface()
	}
	return result
}

func (manager *defaultDocumentManager) doRemove(document interface{}) error {
	metadata, ok := manager.metadatas[reflect.TypeOf(document)]
	if !ok {
		return ErrDocumentNotRegistered
	}
	Value := reflect.Indirect(reflect.ValueOf(document))
	Map := manager.structToMap(document)
	if metadata.hasRelation() {
		for _, field := range metadata.getFieldsWithRelation() {
			if field.relation.cascade == all || field.relation.cascade == remove {
				switch field.relation.relation {
				case referenceMany:
					meta, Type := manager.metadatas.findMetadataByCollectionName(field.relation.targetDocument)
					if Type != nil {
						many := Value.FieldByName(field.name)
						for i := 0; i < many.Len(); i++ {
							doc := many.Index(i)
							idField, ok := meta.findIDField()
							if !ok {
								continue
							}
							id := doc.Elem().FieldByName(idField.name)
							if !isZero(id.Interface()) {
								manager.tasks[doc.Interface()] = del
							}
						}
					}
				case referenceOne:
					// add id of the reference to map , and add the reference in the documents to be saved
					meta, Type := manager.metadatas.findMetadataByCollectionName(field.relation.targetDocument)
					if Type != nil {
						one := Value.FieldByName(field.name)
						if isZero(one.Interface()) {
							continue
						}
						idField, ok := meta.findIDField()
						if !ok {
							continue
						}
						id := one.Elem().FieldByName(idField.name)
						if !isZero(id.Interface()) {
							manager.tasks[one.Interface()] = del
						}
					}
				}
			}
		}
	}
	err := manager.database.C(metadata.targetDocument).RemoveId(Map["_id"])
	if err != nil {
		return err
	}
	// set the id to a zero value
	manager.metadatas.setIDForValue(document, zeroObjectID)
	manager.log(fmt.Sprintf("Removed document with id '%s' from collection '%s' ", Map["_id"], metadata.targetDocument))
	return nil
}

func (manager *defaultDocumentManager) doPersist(document interface{}) error {
	metadata, ok := manager.metadatas[reflect.TypeOf(document)]
	if !ok {
		return ErrDocumentNotRegistered
	}
	Value := reflect.Indirect(reflect.ValueOf(document))
	Map := manager.structToMap(document)
	if metadata.hasRelation() {
		for _, field := range metadata.getFieldsWithRelation() {
			if field.relation.mapped != mappedBy {
				switch field.relation.relation {
				case referenceMany:
					objectIDs := []bson.ObjectId{}
					meta, Type := manager.metadatas.findMetadataByCollectionName(field.relation.targetDocument)
					if Type != nil {
						many := Value.FieldByName(field.name)
						for i := 0; i < many.Len(); i++ {
							doc := many.Index(i)
							idField, ok := meta.findIDField()
							if !ok {
								continue
							}
							id := doc.Elem().FieldByName(idField.name)
							if isZero(id.Interface()) {
								doc.Elem().FieldByName(idField.name).Set(reflect.ValueOf(bson.NewObjectId()))
							}
							objectIDs = append(objectIDs, doc.Elem().FieldByName(idField.name).Interface().(bson.ObjectId))
							if field.relation.cascade == all || field.relation.cascade == persist {
								manager.tasks[doc.Interface()] = insert
							}
						}
					}
					Map[field.key] = objectIDs
				case referenceOne:
					// add id of the reference to map , and add the reference in the documents to be saved
					meta, Type := manager.metadatas.findMetadataByCollectionName(field.relation.targetDocument)
					if Type != nil {
						one := Value.FieldByName(field.name)
						if isZero(one.Interface()) {
							continue
						}
						idField, ok := meta.findIDField()
						if !ok {
							continue
						}
						id := one.Elem().FieldByName(idField.name)
						if isZero(id.Interface()) {
							one.Elem().FieldByName(idField.name).Set(reflect.ValueOf(bson.NewObjectId()))
						}
						if field.relation.cascade == all || field.relation.cascade == persist {
							manager.tasks[one.Interface()] = insert
						}
						Map[field.key] = one.Elem().FieldByName(idField.name).Interface().(bson.ObjectId)
					}
				}
			}
		}
	}
	id := Map["_id"]
	if changeInfo, err := manager.database.C(metadata.targetDocument).UpsertId(id, bson.M{"$set": stripID(Map)}); err != nil {
		return err
	} else {
		manager.log(fmt.Sprintf("Persisted document with id '%s' from collection '%s' , %+v ", id, metadata.targetDocument, changeInfo))
	}
	return nil
}

func (manager *defaultDocumentManager) resolveRelations(documents interface{}, selectedFields ...string) error {
	// this operation is recursive so we need to keep track of the documents than have already
	// been fetched from the DB by their (unique) objectIDs.
	// the relations are resolved recursively. When no relation needs to be resolved or if an error occurs, return.

	// if documents is a pointer to struct instead of a slice then
	// creates a slice of type documents and add document to documents
	if reflect.TypeOf(documents).Kind() == reflect.Ptr {
		if !(reflect.TypeOf(documents).Elem().Kind() == reflect.Array || reflect.TypeOf(documents).Elem().Kind() == reflect.Slice) {
			doc := documents
			slice := reflect.New(reflect.SliceOf(reflect.TypeOf(doc)))
			sliceContent := reflect.Append(slice.Elem(), reflect.ValueOf(doc))
			slice.Elem().Set(sliceContent)
			documents = slice.Interface()
		}
	}
	return manager.doResolveRelations(documents, map[bson.ObjectId]interface{}{}, selectedFields...)
}

func (manager *defaultDocumentManager) doResolveRelations(documents interface{}, fetchedDocuments map[bson.ObjectId]interface{}, selectedFields ...string) error {
	manager.log("Resolving all relations for :", reflect.TypeOf(documents))
	// top is a flag denoting whether it is the first recursive iteration of resolve or not
	top := len(fetchedDocuments) == 0
	Pointer := reflect.ValueOf(documents)
	// expect a pointer
	if Pointer.Kind() != reflect.Ptr {
		return ErrNotAPointer
	}
	Collection := Pointer.Elem()
	// expect an array or a slice
	if kind := Collection.Kind(); kind != reflect.Array && kind != reflect.Slice {
		return ErrNotAnArray
	}
	// find metadata for collection element type
	meta, err := manager.metadatas.getMetadatas(Collection.Type().Elem())
	if err != nil {
		return err
	}
	// get an []reflect.Value so it is easy to iterate on reflect.Value
	sourceValues := convertValueToArrayOfValues(Collection)
	// key values by bson.Object so they are easier to look up
	sourceValuesKeyedBySourceID := keyValuesByObjectID(sourceValues, func(val reflect.Value) bson.ObjectId {
		id, _ := manager.metadatas.getDocumentID(val.Interface())
		return id
	})
	// add values to previously fetched objects
	for objectID, value := range sourceValuesKeyedBySourceID {
		fetchedDocuments[objectID] = value.Interface()
	}
	// if the metadata has relations
	if meta.hasRelation() {
		// get all document ids
		documentIds := getKeys(sourceValuesKeyedBySourceID)
		// for each field that has a relation
		manager.log(fmt.Sprintf("Found %d fields with relation", len(meta.getFieldsWithRelation())))
		for _, field := range meta.getFieldsWithRelation() {
			// if field.key not in selected fields and selected fields length > 0
			// do not check the field for relations
			if len(selectedFields) > 0 && indexOfString(selectedFields, field.name) < 0 {
				continue
			}
			// continue to the next field with relation if load is lazy and we are resolving relationships of related documents
			if !top && field.relation.load == lazy {
				continue
			}
			manager.log("\tRelation for field : ", field.name, field.relation.relation, field.relation.targetDocument, field.relation.mapped, field.relation.mappedField)
			switch field.relation.relation {

			case referenceMany:

				switch field.relation.mapped {

				case mappedBy:
					{ // all relations for referenceMany/mappedBy

						relatedDocs := docs{}
						relatedMetadata, relatedType := manager.metadatas.findMetadataByCollectionName(field.relation.targetDocument)
						if relatedType == nil {
							return ErrDocumentNotRegistered
						}
						relatedField, ok := relatedMetadata.findField(field.relation.mappedField)
						if !ok {
							return ErrFieldNotFound
						}
						if err = manager.GetDB().C(relatedMetadata.targetDocument).Find(bson.M{relatedField.key: bson.M{"$in": documentIds}}).Select(bson.M{"_id": 1, relatedField.key: 1}).All(&relatedDocs); err != nil && err != mgo.ErrNotFound {
							return err
						}
						manager.log("\tnumber of documents found in the db", len(relatedDocs))

						relatedDocsMappedBySourceId := map[bson.ObjectId][]map[string]interface{}{}
						// 2 cases , the difference between them is wether one has to iterate through objectIds
						// or mutliple arrays of objectIds
						switch relatedField.relation.relation {
						case referenceMany:
							// iterate through docs containing arrays of object ids
							for _, doc := range relatedDocs {
								for _, id := range doc[relatedField.key].([]interface{}) {
									relatedDocsMappedBySourceId[id.(bson.ObjectId)] = append(relatedDocsMappedBySourceId[id.(bson.ObjectId)], doc)
								}
							}
						case referenceOne:
							// iterate through docs containing object ids
							for _, doc := range relatedDocs {
								relatedDocsMappedBySourceId[doc[relatedField.key].(bson.ObjectId)] = append(relatedDocsMappedBySourceId[doc[relatedField.key].(bson.ObjectId)], doc)
							}
						}
						relatedCollection := reflect.New(reflect.SliceOf(relatedType))
						// filter out documents that are already in memory
						relatedIds := filter(relatedDocs.getIds(), func(id bson.ObjectId) bool {
							_, ok := fetchedDocuments[id]
							return !ok
						})
						if err = manager.GetDB().C(relatedMetadata.targetDocument).Find(bson.M{"_id": bson.M{"$in": relatedIds}}).All(relatedCollection.Interface()); err != nil && err != mgo.ErrNotFound {
							return err
						}
						relatedDocsMappedById := map[bson.ObjectId]reflect.Value{}
						for i := 0; i < relatedCollection.Elem().Len(); i++ {
							value := relatedCollection.Elem().Index(i)
							id := value.Elem().FieldByName(relatedMetadata.idField).Interface().(bson.ObjectId)
							relatedDocsMappedById[id] = value
						}
						for sourceId, value := range sourceValuesKeyedBySourceID {
							if docs, ok := relatedDocsMappedBySourceId[sourceId]; ok {
								for _, doc := range docs {
									id := doc["_id"].(bson.ObjectId)
									// search in docs that have already been fetched in the previous iteration of resolve
									if v, ok := fetchedDocuments[id]; ok {
										value.Elem().FieldByName(field.name).Set(reflect.Append(value.Elem().FieldByName(field.name), reflect.ValueOf(v)))
										continue
									}
									// search in the documents that have just been fetched
									if v, ok := relatedDocsMappedById[id]; ok {
										// finally append the fetched document to the slice of the field in which the referenceMany/mappedBy
										// relation was defined
										value.Elem().FieldByName(field.name).Set(reflect.Append(value.Elem().FieldByName(field.name), v))
									}
								}
							}
						}
						// resolve relations for the related documents we just fetched
						if err = manager.doResolveRelations(relatedCollection.Interface(), fetchedDocuments); err != nil {
							return err
						}
					}
				default:
					{ // all relations for referenceMany/inversedBy

						// the documents reference many related documents
						results := []map[string]interface{}{}
						if err = manager.GetDB().C(meta.targetDocument).Find(bson.M{"_id": bson.M{"$in": documentIds}}).Select(bson.M{field.key: 1, "_id": 1}).All(&results); err != nil {
							return err
						}
						resultsKeyedByObjectID := keyResultsBySourceID(results, func(result map[string]interface{}) bson.ObjectId {
							return result["_id"].(bson.ObjectId)
						})

						// let's see if some related documents have already been fetched
						for objectID, result := range resultsKeyedByObjectID {
							value := sourceValuesKeyedBySourceID[objectID]
							// if field is empty continue
							if _, ok := result[field.key]; !ok {
								continue
							}
							// otherwise iterate
							for _, relatedID := range result[field.key].([]interface{}) {
								if document, ok := fetchedDocuments[relatedID.(bson.ObjectId)]; ok {
									value.Elem().FieldByName(field.name).Set(reflect.Append(value.Elem().FieldByName(field.name), reflect.ValueOf(document)))
								}
							}
						}
						// let's filter out already existing related documents by objectID
						relatedObjectIds := filter(mapInterfacesToObjectIds(flatten(mapResultsToInterfaces(results, func(result map[string]interface{}) []interface{} {
							if _, ok := result[field.key]; !ok {
								return []interface{}{}
							}
							return result[field.key].([]interface{})
						}),
						), func(i interface{}) bson.ObjectId {
							return i.(bson.ObjectId)
						}),
							func(id bson.ObjectId) bool {
								_, ok := fetchedDocuments[id]
								return !ok
							})
						// if there is no related document to fetch, continue
						if len(relatedObjectIds) == 0 {
							continue
						}
						_, relatedType := manager.metadatas.findMetadataByCollectionName(field.relation.targetDocument)
						if relatedType == nil {
							return ErrDocumentNotRegistered
						}
						relatedDocumentValues := reflect.New(reflect.SliceOf(relatedType))
						// fetch the remaining related documents
						if err = manager.GetDB().C(field.relation.targetDocument).Find(bson.M{"_id": bson.M{"$in": relatedObjectIds}}).All(relatedDocumentValues.Interface()); err != nil {
							return err
						}
						for objectID, result := range resultsKeyedByObjectID {
							value := sourceValuesKeyedBySourceID[objectID]
							for _, id := range result[field.key].([]interface{}) {
								for i := 0; i < relatedDocumentValues.Elem().Len(); i++ {
									relatedObjectID, _ := manager.metadatas.getDocumentID(relatedDocumentValues.Elem().Index(i).Interface())
									if id.(bson.ObjectId) == relatedObjectID {
										value.Elem().FieldByName(field.name).Set(reflect.Append(value.Elem().FieldByName(field.name), relatedDocumentValues.Elem().Index(i)))
									}
								}
							}
						}
						// lets resolve the relations of the related documents
						if err = manager.doResolveRelations(relatedDocumentValues.Interface(), fetchedDocuments); err != nil {
							return err
						}
					}
				}
			case referenceOne:

				switch field.relation.mapped {

				case mappedBy:
					{ // all relations for referenceOne/mappedBy

						// first we need to search the owning side for metadata , the owning side is defined by the argument of mappedBy
						relatedMeta, relatedType := manager.metadatas.findMetadataByCollectionName(field.relation.targetDocument)
						if relatedType == nil {
							return ErrDocumentNotRegistered
						}
						relatedDocumentMaps := []map[string]interface{}{}
						// We need the related struct field and the mongodb key of the owning side which holds the reference to the source document
						relatedField, found := relatedMeta.findField(field.relation.mappedField)
						if !found {
							return ErrFieldNotFound
						}
						// we have a list of source document ids, let's fetch the related documents
						if err = manager.GetDB().C(relatedMeta.targetDocument).Find(bson.M{"_id": bson.M{"$nin": documentIds}, relatedField.key: bson.M{"$in": documentIds}}).Select(bson.M{"_id": 1, relatedField.key: 1}).All(&relatedDocumentMaps); err != nil {
							return err
						}
						// 2 cases here. if the related documents reference many then we need to search through an array
						// if the related documents reference one ,then it is a single value
						relatedDocumentsMapsMappedByDocumentID := map[bson.ObjectId]map[string]interface{}{}
						relatedDocumentIds := []bson.ObjectId{}
						switch relatedField.relation.relation {
						case referenceMany:
							for _, relatedDocument := range relatedDocumentMaps {
								// only append to relatedDocumentIds the documents that have not been fetched yet
								if _, ok := fetchedDocuments[relatedDocument["_id"].(bson.ObjectId)]; !ok {
									relatedDocumentIds = append(relatedDocumentIds, relatedDocument["_id"].(bson.ObjectId))
								}
								for _, id := range relatedDocument[relatedField.key].([]interface{}) {
									relatedDocumentsMapsMappedByDocumentID[id.(bson.ObjectId)] = relatedDocument
								}
							}
						default:
							for _, relatedDocument := range relatedDocumentMaps {
								// only append to relatedDocumentIds the documents that have not been fetched yet
								if _, ok := fetchedDocuments[relatedDocument["_id"].(bson.ObjectId)]; !ok {
									relatedDocumentIds = append(relatedDocumentIds, relatedDocument["_id"].(bson.ObjectId))
								}
								relatedDocumentsMapsMappedByDocumentID[relatedDocument[relatedField.key].(bson.ObjectId)] = relatedDocument
							}
						}

						// let's load the actual related documents fully typed
						relatedDocuments := reflect.New(reflect.SliceOf(relatedType))
						if err = manager.GetDB().C(relatedMeta.targetDocument).Find(bson.M{"_id": bson.M{"$in": relatedDocumentIds}}).All(relatedDocuments.Interface()); err != nil && err != mgo.ErrNotFound {
							return err
						}
						relatedDocumentsMappedByDocumentID := map[bson.ObjectId]reflect.Value{}
						// let's first add the documents that have already been fetched
						for documentId, relatedDocumentMap := range relatedDocumentsMapsMappedByDocumentID {
							if document, ok := fetchedDocuments[relatedDocumentMap["_id"].(bson.ObjectId)]; ok {
								relatedDocumentsMappedByDocumentID[documentId] = reflect.ValueOf(document)
							}
						}
						// let's now add the new related documents we just fetched
						for i := 0; i < relatedDocuments.Elem().Len(); i++ {
							for documentId, relatedDocumentMap := range relatedDocumentsMapsMappedByDocumentID {
								if relatedDocumentMap["_id"].(bson.ObjectId) == relatedDocuments.Elem().Index(i).Elem().FieldByName(relatedMeta.idField).Interface().(bson.ObjectId) {
									relatedDocumentsMappedByDocumentID[documentId] = relatedDocuments.Elem().Index(i)
								}
							}
						}
						// let's now add each related mapped document
						for documentID, value := range sourceValuesKeyedBySourceID {
							// if related document exists
							if relatedDocument, ok := relatedDocumentsMappedByDocumentID[documentID]; ok {
								value.Elem().FieldByName(field.name).Set(relatedDocument)
							}
						}
						// let's resolve the possible relations in the related documents we just fetched
						if err = manager.doResolveRelations(relatedDocuments.Interface(), fetchedDocuments); err != nil {
							return err
						}
					}
				default:
					{ // all relations for referenceOne/inversedBy

						// the documents reference one related document
						results := []map[string]interface{}{}
						if err = manager.GetDB().C(meta.targetDocument).Find(bson.M{field.key: bson.M{"$exists": true}, "_id": bson.M{"$in": documentIds}}).Select(bson.M{field.key: 1, "_id": 1}).All(&results); err != nil && err != mgo.ErrNotFound {
							return err
						}
						resultsKeyedByObjectID := keyResultsBySourceID(results, func(result map[string]interface{}) bson.ObjectId {
							return result["_id"].(bson.ObjectId)
						})

						// search in fetched documents if the relation can already be satisified
						// if yes then set the field of the related doc to the fetched document
						for objectID, result := range resultsKeyedByObjectID {
							if relatedObjectID, ok := result[field.key].(bson.ObjectId); ok {
								if document, ok := fetchedDocuments[relatedObjectID]; ok {
									sourceValuesKeyedBySourceID[objectID].Elem().FieldByName(field.name).Set(reflect.ValueOf(document))
								}
							}
						}
						// we don't need the object ids that have already been fetched
						relatedObjectIds := filter(mapResultsToRelatedObjectIds(results, func(result map[string]interface{}) bson.ObjectId {
							return result[field.key].(bson.ObjectId)
						}), func(id bson.ObjectId) bool {
							_, ok := fetchedDocuments[id]
							return !ok
						})
						// if there is no related document left to fetch , continue
						if len(relatedObjectIds) == 0 {
							continue
						}
						relatedMeta, relatedType := manager.metadatas.findMetadataByCollectionName(field.relation.targetDocument)
						if relatedType == nil {
							return ErrDocumentNotRegistered
						}
						relatedDocumentValues := reflect.New(reflect.SliceOf(relatedType))
						// fetch the remaining documents from the db
						if err = manager.GetDB().C(field.relation.targetDocument).Find(bson.M{"_id": bson.M{"$in": relatedObjectIds}}).All(relatedDocumentValues.Interface()); err != nil && err != mgo.ErrNotFound {
							return err
						}
						relatedDocumentValuesKeyedByObjectID := keyRelatedResultsByObjectID(func() []reflect.Value {
							// transform reflect.Value into []reflect.Value so it can be iterated more easily
							values := []reflect.Value{}
							for i := 0; i < relatedDocumentValues.Elem().Len(); i++ {
								values = append(values, relatedDocumentValues.Elem().Index(i))
							}
							return values
						}(), func(value reflect.Value) bson.ObjectId {
							//println(relatedMeta.idField)
							//println(value.Elem().FieldByName(relatedMeta.idField).Interface().(bson.ObjectId))
							return value.Elem().FieldByName(relatedMeta.idField).Interface().(bson.ObjectId)
						})
						for id, value := range sourceValuesKeyedBySourceID {
							result := resultsKeyedByObjectID[id]
							relatedID := result[field.key].(bson.ObjectId)
							relatedResult := relatedDocumentValuesKeyedByObjectID[relatedID]
							value.Elem().FieldByName(field.name).Set(relatedResult)
						}
						// lets resolve the relations of the related documents
						if err = manager.doResolveRelations(relatedDocumentValues.Interface(), fetchedDocuments); err != nil {
							return err
						}
					}
				}
			}
		}
	}
	return nil
}

// metadata describes how fields of a struct are
// persisted in the DB, if they are indexed and
// their relationships
type metadata struct {
	// targetDocument is the collection associated
	// with the metadata
	targetDocument string
	// structType is the go type associated with
	// the metadata
	structType reflect.Type
	// idField is the struct field name of the field holding
	// the mongo id
	idField string
	// idFkey is the document key holding the mongo id
	idKey string
	// fields are metadatas for struct fields
	fields []field
}

func (meta metadata) String() string {
	return metadataToString(meta)

}

func (meta metadata) findField(fieldname string) (f field, found bool) {
	for _, field := range meta.fields {
		if field.name == fieldname {
			return field, true
		}
	}
	return f, false
}

// hasFieldWithIndex returns true if a field has an index
func (meta metadata) hasFieldWithIndex() bool {
	for _, field := range meta.fields {
		if field.index == true {
			return true
		}
	}
	return false
}

// findFieldsWithIndex returns the fields with an index definition
func (meta metadata) findFieldsWithIndex() []field {
	fieldsWithIndex := []field{}
	for _, field := range meta.fields {
		if field.index == true {
			fieldsWithIndex = append(fieldsWithIndex, field)
		}
	}
	return fieldsWithIndex
}

// getIndexes create a list of indexes from the metadata
func (meta metadata) getIndexes() []mgo.Index {
	indexes := []mgo.Index{}
	for _, field := range meta.findFieldsWithIndex() {
		indexes = append(indexes, mgo.Index{Key: []string{field.key}, Unique: field.unique})
	}
	return indexes
}

// hasFieldWithComposite returns true if a field has an composite index
func (meta metadata) hasFieldWithComposite() bool {
	for _, field := range meta.fields {
		if field.composite == true {
			return true
		}
	}
	return false
}

// findFieldsWithComposite returns the fields with an composite index definition
func (meta metadata) findFieldsWithComposite() []field {
	fieldsWithComposite := []field{}
	for _, field := range meta.fields {
		if field.composite == true {
			fieldsWithComposite = append(fieldsWithComposite, field)
		}
	}
	return fieldsWithComposite
}

// getComposites create a list of indexes from the metadata
func (meta metadata) getComposites() []mgo.Index {
	indexes := []mgo.Index{}
	keys := []string{}
	for _, field := range meta.findFieldsWithComposite() {
		keys = append(keys,field.key)
	}
	indexes = append(indexes, mgo.Index{Key: keys, Unique: true})
	return indexes
}

func (meta metadata) findIDField() (f field, found bool) {
	for _, field := range meta.fields {
		if field.key == "_id" {
			return field, true
		}
	}
	return
}

// hasRelation returns true if one of the fields has a relation
func (meta metadata) hasRelation() bool {
	for _, field := range meta.fields {
		if field.hasRelation() {
			return true
		}
	}
	return false
}

// getFieldsWithRelation returns a collection of fields with relations
func (meta metadata) getFieldsWithRelation() []field {
	fieldsWithRelation := []field{}
	for _, field := range meta.fields {
		if field.hasRelation() {
			fieldsWithRelation = append(fieldsWithRelation, field)
		}
	}
	return fieldsWithRelation
}

// metadatas is a helper type
// used when working with a list of metadatas
// it provides several methods to look up on or extract
// different informations from the metadatas it lists
type metadatas map[reflect.Type]metadata

// GetMetadatas returns the metada for a given type
func (metas metadatas) getMetadatas(Type reflect.Type) (metadata, error) {
	if meta, ok := metas[Type]; !ok {
		return zeroMetadata, ErrDocumentNotRegistered
	} else {
		return meta, nil
	}

}

func (metas metadatas) String() string {
	return fmt.Sprintf("%+v", metas)
}

func (metas metadatas) setIDForValue(document interface{}, id bson.ObjectId) error {
	Value := reflect.ValueOf(document)
	meta, ok := metas[Value.Type()]
	if !ok {
		return ErrDocumentNotRegistered
	}
	Value.Elem().FieldByName(meta.idField).Set(reflect.ValueOf(id))
	return nil
}

// GetIDForValue returns the value of the id field for document
func (metas metadatas) getDocumentID(document interface{}) (id bson.ObjectId, err error) {
	Value := reflect.ValueOf(document)
	meta, ok := metas[Value.Type()]
	if !ok {
		return id, ErrDocumentNotRegistered
	}
	idFields, ok := meta.findIDField()
	if !ok {
		return id, ErrIDFieldNotFound
	}
	return Value.Elem().FieldByName(idFields.name).Interface().(bson.ObjectId), nil

}

func (metas metadatas) findMetadataByCollectionName(name string) (metadata, reflect.Type) {
	for Type, meta := range metas {
		if meta.targetDocument == name {
			return meta, Type
		}
	}
	return zeroMetadata, nil
}

type field struct {
	// index is a index composite
	composite bool
	// index is a index
	index bool
	// unique is a unique index
	unique bool
	// mongodb document key
	key string
	// struct field name
	name string
	// whether to omit 0 values
	omitempty bool
	relation  relation
	ignore    bool
}

func (f field) String() string {
	return fmt.Sprintf(" key:'%s', name:'%s', omitempty:'%v' ignore:'%v' relation:%s ",
		f.key, f.name, f.omitempty, f.ignore, f.relation)

}

func (f field) hasRelation() bool {
	return f.relation != zeroRelation
}

// relation defines a relationship between the source document
// and a related document
type relation struct {
	// load is how the related relations should be loaded.
	// When a document is fetched, all direct relationships are resolved.
	// If load is eager then relations on the related documents are loaded as well.
	// If load is lazy then they are not loaded and ResolveRelation must be called explicitly
	// load defaults to lazy
	load load
	// relation is the type of relation.
	// it can either be referenceOne(has one) or referenceMany(has many)
	relation relationType

	// targetDocument is the related document
	targetDocument string

	// cascade sets how related datas is automatically persisted or removed
	// it can be either all, persist or remove
	cascade cascade

	// mapped is either mappedBy or inversedBy or 0
	// if mappedBy then the type will not old any reference
	// to the targetDocument
	mapped relationMap

	// mappedField is the field on the related document which holds
	// the source document
	mappedField string

	// idField is the field that holds the related document id or ids
	idStorageField string
}

func (r relation) String() string {
	if isZero(r) {
		return "{}"
	}
	return fmt.Sprintf("{ relation: '%s', targetDocument: '%s', cascade: '%v', mapped: '%s', mappedField: '%v' ,idField '%v' } ",
		r.relation, r.targetDocument, r.cascade, r.mapped, r.mappedField, r.idStorageField)
}

type relationMap int

const (
	_ relationMap = iota
	mappedBy
	inversedBy
)

func (m relationMap) String() string {
	switch m {
	case mappedBy:
		return "mappedBy"
	case inversedBy:
		return "inversedBy"
	default:
		return ""
	}
}

type relationType int

const (
	_ relationType = iota
	referenceMany
	referenceOne
)

func (Type relationType) String() string {
	switch Type {
	case referenceMany:
		return "referenceMany"
	case referenceOne:
		return "referenceOne"
	}
	return ""
}

// load defines whether a relationship
// is lazyly or eagerly fetched when related documents
// have their own relationships resolved
type load int

const (
	lazy load = iota
	eager
)

// cascade sets the behavior of related documents when
// a document is removed or saved. Related documents can be automatically
// removed or saved when the source document is removed or saved.
type cascade int

const (
	_ cascade = iota
	all
	persist
	remove
)

// task is a pending task which is executed when DocumentManager.Flush
// is called.
type task int

const (
	del task = iota
	insert
	update
)

type tasks map[interface{}]task

func (t tasks) pop() (interface{}, task) {
	for value, task := range t {
		delete(t, value)
		return value, task
	}
	return nil, 0
}

// documents helps deal with fetched documents
// when resolving relations
type docs []map[string]interface{}

// mapBy maps docs by a key 'key'.
// if a doc has no existing key 'key', then it will not be part of result
func (d docs) mapBy(key string) (result map[interface{}]map[string]interface{}) {
	result = map[interface{}]map[string]interface{}{}
	for _, doc := range d {
		if _, ok := doc[key]; !ok {
			continue
		}
		result[doc[key]] = doc
	}
	return
}

// mapByID maps docs by objectId
// an optional key can be provided, it defaults to _id
func (d docs) mapByID(key ...string) (result map[bson.ObjectId]map[string]interface{}) {
	result = map[bson.ObjectId]map[string]interface{}{}
	if len(key) == 0 {
		key = []string{"_id"}
	}
	for _, doc := range d {
		if _, ok := doc[key[0]]; !ok {
			continue
		}
		result[doc[key[0]].(bson.ObjectId)] = doc
	}
	return
}

// getIds return an array of document id
func (d docs) getIds() (ids []bson.ObjectId) {
	for _, doc := range d {
		ids = append(ids, doc["_id"].(bson.ObjectId))
	}
	return
}

func convertValueToArrayOfValues(value reflect.Value) (arrayOfValues []reflect.Value) {
	for i := 0; i < value.Len(); i++ {
		arrayOfValues = append(arrayOfValues, value.Index(i))
	}
	return
}

// stripID removes the _id key of a map
func stripID(Map map[string]interface{}) map[string]interface{} {
	delete(Map, "_id")
	return Map
}

func isZero(value interface{}) bool {
	Value := reflect.ValueOf(value)
	if Value.Kind() == reflect.Array || Value.Kind() == reflect.Slice {
		if Value.Len() == 0 {
			return true
		}
		return false
	}
	return Value.Interface() == reflect.Zero(Value.Type()).Interface()
}

func isPointer(value interface{}) bool {
	return reflect.ValueOf(value).Kind() == reflect.Ptr
}

func isStruct(value interface{}) bool {
	return reflect.ValueOf(value).Kind() == reflect.Struct
}

func isIterable(value interface{}) bool {
	kind := reflect.ValueOf(value).Kind()
	return kind == reflect.Array || kind == reflect.Slice
}

// getTypeMetadatas takes a pointer to struct and returns the metadata
// for the struct or an error if the struct tag is invalid.
func getTypeMetadatas(value interface{}) (meta metadata, err error) {
	Value := reflect.Indirect(reflect.ValueOf(value))
	Type := Value.Type()
	// for each field in struct, read its struct tag and
	// create a metadata for the field if needed
	for i := 0; i < Value.NumField(); i++ {
		Field := Type.Field(i)
		MetaField := field{name: Field.Name, key: strings.ToLower(Field.Name)}
		// check bson struct tag and extract the document key
		Tag := Field.Tag.Get("bson")
		parts := strings.Split(Tag, ",")
		if len(parts) > 0 {
			if key := strings.TrimSpace(parts[0]); key != "" {
				MetaField.key = key
				if key == "_id" {
					meta.idField = Field.Name
					meta.idKey = "_id"
				}
				// ignore the field and continue
				if key == "-" {
					MetaField.ignore = true
					continue
				}
			}
		}
		if len(parts) > 1 {
			if part := strings.TrimSpace(parts[1]); part == "omitempty" {
				MetaField.omitempty = true
			}
		}
		Tag = Field.Tag.Get("odm")
		if Tag == "-" {
			MetaField.ignore = true
			continue
		}
		parser := tag.NewParser(strings.NewReader(Tag))
		var definitions []*tag.Definition
		definitions, err = parser.Parse()
		if err != nil {
			return meta, err
		}
		for _, definition := range definitions {
			switch strings.ToLower(definition.Name) {

			case "id":
				meta.idField = Field.Name
			case "omitempty":
				MetaField.omitempty = true
			case "index":
				MetaField.index = true
				for _, parameter := range definition.Parameters {
					switch toLower(parameter.Key) {
					case "unique":
						MetaField.unique = true
					}
				}
			case "composite":
				MetaField.composite = true
			case "referencemany", "referenceone":
				Relation := relation{}
				switch strings.ToLower(definition.Name) {
				case "referencemany":
					Relation.relation = referenceMany
					MetaField.key = "odm:" + strings.ToLower(Field.Name) + "ids"
				case "referenceone":
					Relation.relation = referenceOne
					MetaField.key = "odm:" + strings.ToLower(Field.Name) + "id"
				}
				for _, parameter := range definition.Parameters {
					switch strings.ToLower(parameter.Key) {
					case "mappedby":
						Relation.mapped = mappedBy
						Relation.mappedField = parameter.Value
					case "inversedby":
						Relation.mapped = inversedBy
						Relation.mappedField = parameter.Value
					case "targetdocument":
						Relation.targetDocument = parameter.Value
					case "cascade":
						switch strings.ToLower(parameter.Value) {
						case "persist":
							Relation.cascade = persist
						case "remove":
							Relation.cascade = remove
						case "all":
							Relation.cascade = all
						}
					case "storeid":
						Relation.idStorageField = parameter.Value
					case "load":
						switch strings.ToLower(parameter.Value) {
						case "eager":
							Relation.load = eager

						}
					default:
						return meta, ErrInvalidAnnotation
					}
					MetaField.relation = Relation
				}
			default:
				return meta, ErrInvalidAnnotation
			}
		}
		// remove index definition if field has a relation
		if MetaField.index == true && MetaField.hasRelation() {
			MetaField.index = false
		}
		// use a specific field to store related ids
		if MetaField.relation.idStorageField != "" {

			if key := resolveKeyForField(Type, MetaField.relation.idStorageField); key != "" {

				MetaField.key = key
			}
		}
		meta.fields = append(meta.fields, MetaField)
	}
	return
}

// toLower is a shortcurt to strings.ToLower method
func toLower(s string) string {
	return strings.ToLower(s)
}

// resolveKeyForField either returns lowercase version of the name if the field
// was found or the key found in a bson struct tag or "" if the field wasn't found
func resolveKeyForField(Struct reflect.Type, name string) string {
	if f, ok := Struct.FieldByName(name); ok {
		tag := f.Tag.Get("bson")
		parts := strings.Split(tag, ",")
		if len(parts) > 0 && parts[0] != "" && parts[0] != "-" {
			return parts[0]
		} else {
			return strings.ToLower(f.Name)
		}
	}
	return ""
}

func metadataToString(meta metadata) string {
	result := "metadata : {"
	result += "collectionName: '" + meta.targetDocument + "', "
	result += "idField: '" + meta.idField + "' "
	result += "fields :[\n"
	for i, field := range meta.fields {
		if i > 0 {
			result += ",\n "
		}
		result += "{" + field.String() + "}"
	}
	return result + "\n]}\n"
}

var (
	keyValuesByObjectID func(collection []reflect.Value, selector func(reflect.Value) bson.ObjectId) map[bson.ObjectId]reflect.Value
	_                   = funcs.Must(funcs.MakeKeyBy(&keyValuesByObjectID))

	getKeys func(map[bson.ObjectId]reflect.Value) []bson.ObjectId
	_       = funcs.Must(funcs.MakeGetKeys(&getKeys))

	flatten func([][]interface{}) []interface{}
	_       = funcs.Must(funcs.MakeFlatten(&flatten))

	mapResultsToInterfaces func([]map[string]interface{}, func(map[string]interface{}) []interface{}) [][]interface{}
	_                      = funcs.Must(funcs.MakeMap(&mapResultsToInterfaces))

	keyResultsBySourceID func(results []map[string]interface{}, mapper func(result map[string]interface{}) (id bson.ObjectId)) map[bson.ObjectId]map[string]interface{}
	_                    = funcs.Must(funcs.MakeKeyBy(&keyResultsBySourceID))

	mapResultsToRelatedObjectIds func(results []map[string]interface{}, mapper func(result map[string]interface{}) bson.ObjectId) []bson.ObjectId
	_                            = funcs.Must(funcs.MakeMap(&mapResultsToRelatedObjectIds))

	keyRelatedResultsByObjectID func(results []reflect.Value, mapper func(result reflect.Value) bson.ObjectId) map[bson.ObjectId]reflect.Value
	_                           = funcs.Must(funcs.MakeKeyBy(&keyRelatedResultsByObjectID))

	mapInterfacesToObjectIds func([]interface{}, func(interface{}) bson.ObjectId) []bson.ObjectId
	_                        = funcs.Must(funcs.MakeMap(&mapInterfacesToObjectIds))

	filter func([]bson.ObjectId, func(id bson.ObjectId) bool) []bson.ObjectId
	_      = funcs.Must(funcs.MakeFilter(&filter))

	indexOf func([]interface{}, interface{}) int
	_       = funcs.Must(funcs.MakeIndexOf(&indexOf))

	indexOfString func([]string, string) int
	_             = funcs.Must(funcs.MakeIndexOf(&indexOfString))
)
