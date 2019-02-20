package communications

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/open-horizon/edge-sync-service/common"
	"github.com/open-horizon/edge-sync-service/core/dataURI"
	"github.com/open-horizon/edge-sync-service/core/leader"
	"github.com/open-horizon/edge-sync-service/core/storage"
	"github.com/open-horizon/edge-utilities/logger"
	"github.com/open-horizon/edge-utilities/logger/log"
	"github.com/open-horizon/edge-utilities/logger/trace"
)

type notificationHandlerError struct {
	message string
}

func (e *notificationHandlerError) Error() string {
	return e.message
}

type notificationChunksInfo struct {
	maxRequestedOffset int64
	maxReceivedOffset  int64
	receivedDataSize   int64
	chunkResendTimes   map[int64]int64 // This map holds resend time per in-flight chunk (keyed by the offset)
	chunksReceived     []byte          // This byte array holds a bit per chunk indicating its arrival
	chunkSize          int
	resendTime         int64
}

var notificationChunks map[string]notificationChunksInfo

func init() {
	notificationChunks = make(map[string]notificationChunksInfo)
}

const numberOfLocks = 256 // This MUST be a power of 2
var objectLocks [numberOfLocks]sync.Mutex

func lockObject(index uint32) {
	objectLocks[index&(numberOfLocks-1)].Lock()
}
func unlockObject(index uint32) {
	objectLocks[index&(numberOfLocks-1)].Unlock()
}

var notificationLock sync.RWMutex

// CSS: handle ESS registration
func handleRegistration(dest common.Destination, persistentStorage bool) common.SyncServiceError {
	if common.Configuration.NodeType == common.ESS {
		return &notificationHandlerError{"ESS cannot register other services"}
	}

	if trace.IsLogging(logger.TRACE) {
		trace.Trace("Handling registration of %s %s\n", dest.DestType, dest.DestID)
	}

	reconnection, err := Store.DestinationExists(dest.DestOrgID, dest.DestType, dest.DestID)
	if err != nil {
		return &notificationHandlerError{fmt.Sprintf("Error in handleRegistration: failed to check destination's existence. Error: %s\n", err)}
	}

	// Add to the destinations list
	if err := Store.StoreDestination(dest); err != nil {
		return &notificationHandlerError{fmt.Sprintf("Error in handleRegistration: failed to store destination. Error: %s\n", err)}
	}

	// Ack
	if err := Comm.RegisterAck(dest); err != nil {
		return &notificationHandlerError{fmt.Sprintf("Error in handleRegistration: failed to send ack. Error: %s\n", err)}
	}

	if reconnection {
		// If a reconnection, go through the notifications and resend those that have not been acknowledged
		if log.IsLogging(logger.INFO) {
			log.Info("Reconnection of: %s %s %s\n", dest.DestOrgID, dest.DestType, dest.DestID)
		}

		if err := resendNotificationsForDestination(dest, !persistentStorage); err != nil {
			return &notificationHandlerError{fmt.Sprintf("Error in handleRegistration. Error: %s\n", err)}
		}
	} else {
		// If a new destination, go through the objects and look for the relevant updates
		if log.IsLogging(logger.INFO) {
			log.Info("New destination: %s %s %s\n", dest.DestOrgID, dest.DestType, dest.DestID)
		}
		objects, err := Store.RetrieveObjects(dest.DestOrgID, dest.DestType, dest.DestID)
		if err != nil {
			return &notificationHandlerError{fmt.Sprintf("Error in handleRegistration. Error: %s\n", err)}
		}

		if len(objects) > 0 {
			destinations := make([]common.Destination, 1)
			destinations[0] = dest
			for _, metaData := range objects {
				if err := SendNotification(metaData, destinations); err != nil {
					return &notificationHandlerError{fmt.Sprintf("Error in handleRegistration. Error: %s\n", err)}
				}
			}
		}
	}

	return nil
}

func handleRegAck() {
	common.Registered = true
}

// Handle a notification about object update
func handleUpdate(metaData common.MetaData, maxInflightChunks int) common.SyncServiceError {
	if trace.IsLogging(logger.TRACE) {
		trace.Trace("Handling update of %s %s\n", metaData.ObjectType, metaData.ObjectID)
	}

	if notification, err := Store.RetrieveNotificationRecord(metaData.DestOrgID, metaData.ObjectType, metaData.ObjectID,
		metaData.OriginType, metaData.OriginID); err == nil && notification != nil {
		if notification.InstanceID >= metaData.InstanceID {
			// This object has been sent already, ignore
			if trace.IsLogging(logger.TRACE) {
				trace.Trace("Ignoring object update of %s %s\n", metaData.ObjectType, metaData.ObjectID)
			}
			return nil
		}
		Store.DeleteNotificationRecords(metaData.DestOrgID, metaData.ObjectType, metaData.ObjectID,
			metaData.OriginType, metaData.OriginID)
		removeNotificationChunksInfo(metaData, metaData.OriginType, metaData.OriginID)
	}

	// Store the object
	status := common.PartiallyReceived
	if metaData.Link != "" || metaData.NoData || metaData.MetaOnly {
		status = common.CompletelyReceived
	}
	if err := Store.StoreObject(metaData, nil, status); err != nil {
		return &notificationHandlerError{fmt.Sprintf("Error in handleUpdate: failed to store object. Error: %s\n", err)}
	}

	// Call Notification module to send notification to object’s sender
	if err := Comm.SendNotificationMessage(common.Updated, metaData.OriginType, metaData.OriginID, metaData.InstanceID,
		&metaData); err != nil {
		return &notificationHandlerError{fmt.Sprintf("Error in handleUpdate: failed to send notification. Error: %s\n", err)}
	}

	if status == common.CompletelyReceived {
		return nil
	}

	lockIndex := common.HashStrings(metaData.DestOrgID, metaData.ObjectType, metaData.ObjectID)
	lockObject(lockIndex)
	defer unlockObject(lockIndex)

	if metaData.ChunkSize <= 0 || metaData.ObjectSize <= 0 {
		if err := Comm.GetData(metaData, 0); err != nil {
			return &notificationHandlerError{fmt.Sprintf("Error in handleUpdate: failed to send notification. Error: %s\n", err)}
		}
	} else {
		var offset int64
		for i := 0; i < maxInflightChunks && offset < metaData.ObjectSize; i++ {
			if err := Comm.GetData(metaData, offset); err != nil {
				return &notificationHandlerError{fmt.Sprintf("Error in handleUpdate: failed to send notification. Error: %s\n", err)}
			}
			offset += int64(metaData.ChunkSize)
		}
	}

	return nil
}

// Handle a notification that an object's update was received by the other side
func handleObjectUpdated(orgID string, objectType string, objectID string, destType string, destID string,
	instanceID int64) common.SyncServiceError {
	if trace.IsLogging(logger.TRACE) {
		trace.Trace("Handling object updated of %s %s\n", objectType, objectID)
	}

	notification, err := Store.RetrieveNotificationRecord(orgID, objectType, objectID, destType, destID)
	if err != nil || notification == nil {
		return &notificationHandlerError{"Error in handleObjectUpdated: no notification to update."}
	}
	if notification.InstanceID != instanceID || (notification.Status != common.Update && notification.Status != common.UpdatePending) {
		// This notification doesn't match the existing notification record, ignore
		if trace.IsLogging(logger.TRACE) {
			trace.Trace("Ignoring object updated of %s %s\n", objectType, objectID)
		}
		return nil
	}

	Store.UpdateNotificationRecord(
		common.Notification{ObjectID: objectID, ObjectType: objectType,
			DestOrgID: orgID, DestID: destID, DestType: destType, Status: common.Updated, InstanceID: instanceID})

	return nil
}

// Handle a notification that an object's update was consumed by the other side
func handleObjectConsumed(orgID string, objectType string, objectID string, destType string, destID string,
	instanceID int64) common.SyncServiceError {
	if trace.IsLogging(logger.TRACE) {
		trace.Trace("Handling object consumed of %s %s\n", objectType, objectID)
	}

	notification, err := Store.RetrieveNotificationRecord(orgID, objectType, objectID, destType, destID)
	if err != nil || notification == nil {
		return &notificationHandlerError{"Error in handleObjectConsumed: no notification to update."}
	}
	if notification.InstanceID != instanceID ||
		(notification.Status != common.Data && notification.Status != common.Updated && notification.Status != common.ReceivedByDestination) {
		// This notification doesn't match the existing notification record, ignore
		if trace.IsLogging(logger.TRACE) {
			trace.Trace("Ignoring object consumed of %s %s\n", objectType, objectID)
		}
		return nil
	}

	metaData, err := Store.RetrieveObject(orgID, objectType, objectID)
	if err != nil {
		return &notificationHandlerError{"Failed to find stored object. Error: " + err.Error()}
	}

	// ESS: delete the object
	if common.Configuration.NodeType == common.ESS {
		if err = Store.DeleteStoredObject(orgID, objectType, objectID); err != nil && log.IsLogging(logger.ERROR) {
			log.Error("Error in handleObjectConsumed: failed to delete stored object. Error: %s\n", err)
		}
		err = Store.DeleteNotificationRecords(orgID, objectType, objectID, "", "")
		if err != nil && log.IsLogging(logger.ERROR) {
			log.Error("Error in handleObjectConsumed: failed to delete notification records. Error: %s\n", err)
		}
		removeNotificationChunksInfo(*metaData, metaData.OriginType, metaData.OriginID)
	} else {
		// Mark that the object was consumed by this destination
		err = Store.UpdateObjectDeliveryStatus(common.Consumed, orgID, objectType, objectID, destType, destID)
		if err != nil && log.IsLogging(logger.ERROR) {
			log.Error("Error in handleObjectConsumed: failed to mark object as delivered to the destination. Error: %s\n", err)
		}
		// Mark the corresponding update notification as "ackconsumed"
		if err := Store.UpdateNotificationRecord(
			common.Notification{ObjectID: objectID, ObjectType: objectType,
				DestOrgID: orgID, DestID: destID, DestType: destType, Status: common.AckConsumed, InstanceID: instanceID},
		); err != nil {
			return &notificationHandlerError{fmt.Sprintf("Error in handleObjectConsumed: failed to update notification record. Error: %s\n", err)}
		}
	}

	// Send ack
	if err := Comm.SendNotificationMessage(common.AckConsumed, destType, destID, instanceID, metaData); err != nil {
		return &notificationHandlerError{fmt.Sprintf("Error in handleObjectConsumed: failed to send notification. Error: %s\n",
			err)}
	}

	return nil
}

// Handle a notification that an object's was marked as consumed by the other side
func handleAckConsumed(orgID string, objectType string, objectID string, destType string, destID string, instanceID int64) common.SyncServiceError {
	if trace.IsLogging(logger.TRACE) {
		trace.Trace("Handling ack consumed of %s %s\n", objectType, objectID)
	}

	notification, err := Store.RetrieveNotificationRecord(orgID, objectType, objectID, destType, destID)
	if err != nil || notification == nil {
		return &notificationHandlerError{"Error in handleAckConsumed: no notification to update."}
	}
	if notification.InstanceID != instanceID || (notification.Status != common.Consumed && notification.Status != common.ConsumedPending) {
		// This notification doesn't match the existing notification record, ignore
		if trace.IsLogging(logger.TRACE) {
			trace.Trace("Ignoring ack consumed of %s %s\n", objectType, objectID)
		}
		return nil
	}

	// Mark the notification as ackconsumed
	if err := Store.UpdateNotificationRecord(
		common.Notification{ObjectID: objectID, ObjectType: objectType,
			DestOrgID: orgID, DestID: destID, DestType: destType, Status: common.AckConsumed, InstanceID: instanceID},
	); err != nil {
		return &notificationHandlerError{fmt.Sprintf("Error in handleAckConsumed: failed to update notification record. Error: %s\n", err)}
	}

	// ESS: delete the object
	if common.Configuration.NodeType == common.ESS {
		err = Store.DeleteStoredObject(orgID, objectType, objectID)
		if err != nil && log.IsLogging(logger.ERROR) {
			log.Error("Error in handleAckConsumed: failed to delete stored object. Error: %s\n", err)
		}
		err = Store.DeleteNotificationRecords(orgID, objectType, objectID, "", "")
		if err != nil && log.IsLogging(logger.ERROR) {
			log.Error("Error in handleAckConsumed: failed to delete notification records. Error: %s\n", err)
		}
	}

	return nil
}

// Handle a notification that an object's update was received by the other side
func handleObjectReceived(orgID string, objectType string, objectID string, destType string, destID string,
	instanceID int64) common.SyncServiceError {
	if trace.IsLogging(logger.TRACE) {
		trace.Trace("Handling object received of %s %s\n", objectType, objectID)
	}

	notification, err := Store.RetrieveNotificationRecord(orgID, objectType, objectID, destType, destID)
	if err != nil || notification == nil {
		return &notificationHandlerError{"Error in handleObjectReceived: no notification to update."}
	}
	if notification.InstanceID != instanceID ||
		(notification.Status != common.Data && notification.Status != common.Updated) {
		// This notification doesn't match the existing notification record, ignore
		if trace.IsLogging(logger.TRACE) {
			trace.Trace("Ignoring object received of %s %s\n", objectType, objectID)
		}
		return nil
	}

	metaData, err := Store.RetrieveObject(orgID, objectType, objectID)
	if err != nil {
		return &notificationHandlerError{"Failed to find stored object. Error: " + err.Error()}
	}

	// Mark that the object was delivered to this destination
	err = Store.UpdateObjectDeliveryStatus(common.Delivered, orgID, objectType, objectID, destType, destID)
	if err != nil && log.IsLogging(logger.ERROR) {
		log.Error("Error in handleObjectReceived: failed to mark object as delivered to the destination. Error: %s\n", err)
	}
	// Mark the corresponding update notification as "received by destination"
	if err := Store.UpdateNotificationRecord(
		common.Notification{ObjectID: objectID, ObjectType: objectType,
			DestOrgID: orgID, DestID: destID, DestType: destType, Status: common.ReceivedByDestination, InstanceID: instanceID},
	); err != nil {
		return &notificationHandlerError{fmt.Sprintf("Error in handleObjectReceived: failed to update notification record. Error: %s\n", err)}
	}

	// Send ack
	if err := Comm.SendNotificationMessage(common.AckReceived, destType, destID, instanceID, metaData); err != nil {
		return &notificationHandlerError{fmt.Sprintf("Error in handleObjectReceived: failed to send notification. Error: %s\n",
			err)}
	}

	return nil
}

// Handle a notification that an object's was marked as received by the other side
func handleAckObjectReceived(orgID string, objectType string, objectID string, destType string, destID string, instanceID int64) common.SyncServiceError {
	if trace.IsLogging(logger.TRACE) {
		trace.Trace("Handling ack received of %s %s\n", objectType, objectID)
	}

	notification, err := Store.RetrieveNotificationRecord(orgID, objectType, objectID, destType, destID)
	if err != nil || notification == nil {
		return &notificationHandlerError{"Error in handleAckObjectReceived: no notification to update."}
	}
	if notification.InstanceID != instanceID || (notification.Status != common.Received && notification.Status != common.ReceivedPending) {
		// This notification doesn't match the existing notification record, ignore
		if trace.IsLogging(logger.TRACE) {
			trace.Trace("Ignoring ack received of %s %s\n", objectType, objectID)
		}
		return nil
	}

	// Mark the notification as ackreceived
	if err := Store.UpdateNotificationRecord(
		common.Notification{ObjectID: objectID, ObjectType: objectType,
			DestOrgID: orgID, DestID: destID, DestType: destType, Status: common.AckReceived, InstanceID: instanceID},
	); err != nil {
		return &notificationHandlerError{fmt.Sprintf("Error in handleAckObjectReceived: failed to update notification record. Error: %s\n", err)}
	}

	return nil
}

// Handle a notification about object delete
func handleDelete(metaData common.MetaData) common.SyncServiceError {
	if trace.IsLogging(logger.TRACE) {
		trace.Trace("Handling delete of %s %s\n", metaData.ObjectType, metaData.ObjectID)
	}

	if err := Store.MarkObjectDeleted(metaData.DestOrgID, metaData.ObjectType, metaData.ObjectID); err != nil {
		if common.Configuration.NodeType == common.ESS && storage.IsNotFound(err) {
			// Failed to update, object doesn't exist, on ESS recreate it (without data)
			metaData.Deleted = true
			if err := Store.StoreObject(metaData, nil, common.ObjDeleted); err != nil {
				return &notificationHandlerError{fmt.Sprintf("Error in handleDelete: failed to recreate deleted object. Error: %s\n", err)}
			}
		} else {
			if trace.IsLogging(logger.TRACE) {
				trace.Trace("In handleDelete: failed to update status of %s %s. Error: %s\n", metaData.ObjectType, metaData.ObjectID, err)
			}

			if common.Configuration.NodeType == common.CSS {
				// On CSS, call Notification module to send Deleted notification to object’s sender
				if err := Comm.SendNotificationMessage(common.Deleted, metaData.OriginType, metaData.OriginID,
					metaData.InstanceID, &metaData); err != nil {
					return &notificationHandlerError{fmt.Sprintf("Error in handleDelete: failed to send notification. Error: %s\n", err)}
				}
			}
		}
	} else {
		// Object exists, remove its data
		err = Store.DeleteStoredData(metaData.DestOrgID, metaData.ObjectType, metaData.ObjectID)
		if err != nil && trace.IsLogging(logger.TRACE) {
			trace.Trace("Error in handleDelete: %s \n", err)
		}
		// Reset expected consumers to remove the object after all consumers delete it
		err = Store.ResetObjectRemainingConsumers(metaData.DestOrgID, metaData.ObjectType, metaData.ObjectID)
		if err != nil && trace.IsLogging(logger.TRACE) {
			trace.Trace("Error in handleDelete: %s \n", err)
		}
	}

	// Delete object's notifications
	Store.DeleteNotificationRecords(metaData.DestOrgID, metaData.ObjectType, metaData.ObjectID, "", "")
	removeNotificationChunksInfo(metaData, metaData.OriginType, metaData.OriginID)

	// Send ack
	if err := Comm.SendNotificationMessage(common.AckDelete, metaData.OriginType, metaData.OriginID, metaData.InstanceID,
		&metaData); err != nil {
		return &notificationHandlerError{fmt.Sprintf("Error in handleDelete: failed to send notification. Error: %s\n", err)}
	}

	return nil
}

// Handle a notification that an object's was marked as deleted by the other side
func handleAckDelete(orgID string, objectType string, objectID string, destType string, destID string, instanceID int64) common.SyncServiceError {
	if trace.IsLogging(logger.TRACE) {
		trace.Trace("Handling ack delete of %s %s\n", objectType, objectID)
	}

	notification, err := Store.RetrieveNotificationRecord(orgID, objectType, objectID, destType, destID)
	if err != nil || notification == nil {
		return &notificationHandlerError{"Error in handleAckConsumed: no notification to update."}
	}
	if notification.InstanceID != instanceID || (notification.Status != common.Delete && notification.Status != common.DeletePending) {
		// This notification doesn't match the existing notification record, ignore
		if trace.IsLogging(logger.TRACE) {
			trace.Trace("Ignoring ack delete of %s %s\n", objectType, objectID)
		}
		return nil
	}

	// Mark the notification as ackdelete
	if err := Store.UpdateNotificationRecord(
		common.Notification{ObjectID: objectID, ObjectType: objectType,
			DestOrgID: orgID, DestID: destID, DestType: destType, Status: common.AckDelete, InstanceID: instanceID},
	); err != nil {
		return &notificationHandlerError{fmt.Sprintf("Error in handleAckDelete: failed to update notification record. Error: %s\n", err)}
	}

	// Delete the object
	return Store.DeleteStoredObject(orgID, objectType, objectID)
}

// Handle a notification that an object was deleted by the other side
func handleObjectDeleted(metaData common.MetaData) common.SyncServiceError {
	if trace.IsLogging(logger.TRACE) {
		trace.Trace("Handling object deleted of %s %s\n", metaData.ObjectType, metaData.ObjectID)
	}

	notification, err := Store.RetrieveNotificationRecord(metaData.DestOrgID, metaData.ObjectType, metaData.ObjectID, metaData.DestType, metaData.DestID)
	if err != nil || notification == nil {
		return &notificationHandlerError{"Error in handleObjectDeleted: no notification to update."}
	}
	if notification.InstanceID != metaData.InstanceID ||
		(notification.Status != common.Delete && notification.Status != common.DeletePending && notification.Status != common.AckDelete) {
		// This notification doesn't match the existing notification record, ignore
		if trace.IsLogging(logger.TRACE) {
			trace.Trace("Ignoring object deleted of %s %s\n", metaData.ObjectType, metaData.ObjectID)
		}
		return nil
	}

	// Delete the notification
	err = Store.DeleteNotificationRecords(metaData.DestOrgID, metaData.ObjectType, metaData.ObjectID, "", "")
	if err != nil && log.IsLogging(logger.ERROR) {
		log.Error("Error in handleAckConsumed: failed to delete notification records. Error: %s\n", err)
	}
	removeNotificationChunksInfo(metaData, metaData.OriginType, metaData.OriginID)

	// Send ack
	if err := Comm.SendNotificationMessage(common.AckDeleted, metaData.DestType, metaData.DestID, metaData.InstanceID,
		&metaData); err != nil {
		return &notificationHandlerError{fmt.Sprintf("Error in handleObjectDeleted: failed to send notification. Error: %s\n", err)}
	}
	return nil
}

// Handle a notification that the object deleted notification was received by the other side
func handleAckObjectDeleted(orgID string, objectType string, objectID string, destType string, destID string, instanceID int64) common.SyncServiceError {
	if trace.IsLogging(logger.TRACE) {
		trace.Trace("Handling ack object deleted of %s %s\n", objectType, objectID)
	}

	notification, err := Store.RetrieveNotificationRecord(orgID, objectType, objectID, destType, destID)
	if err != nil || notification == nil {
		return &notificationHandlerError{"Error in handleAckObjectDeleted: no notification to update."}
	}
	if notification.InstanceID != instanceID || (notification.Status != common.Deleted && notification.Status != common.DeletedPending) {
		// This notification doesn't match the existing notification record, ignore
		if trace.IsLogging(logger.TRACE) {
			trace.Trace("Ignoring ack object deleted of %s %s\n", objectType, objectID)
		}
		return nil
	}

	// Delete the notification
	err = Store.DeleteNotificationRecords(orgID, objectType, objectID, "", "")
	if err != nil && log.IsLogging(logger.ERROR) {
		log.Error("Error in handleAckConsumed: failed to delete notification records. Error: %s\n", err)
	}

	// Delete the object
	if err = Store.DeleteStoredObject(orgID, objectType, objectID); err != nil && log.IsLogging(logger.ERROR) {
		log.Error("Error in objectDeleted: failed to delete stored object. Error: %s\n", err)
	}
	return nil
}

func handleResendRequest(dest common.Destination) common.SyncServiceError {
	if trace.IsLogging(logger.TRACE) {
		trace.Trace("Handling resend objects request for %s/%s/%s\n", dest.DestOrgID, dest.DestType, dest.DestID)
	}

	// Send ack
	if err := Comm.SendAckResendObjects(dest); err != nil {
		return &notificationHandlerError{fmt.Sprintf("Error in handleResendRequest: failed to send ack. Error: %s\n", err)}
	}

	objects, err := Store.RetrieveObjects(dest.DestOrgID, dest.DestType, dest.DestID)
	if err != nil {
		return &notificationHandlerError{fmt.Sprintf("Error in handleResendRequest. Error: %s\n", err)}
	}

	if len(objects) > 0 {
		destinations := make([]common.Destination, 1)
		destinations[0] = dest
		for _, metaData := range objects {
			if err := SendNotification(metaData, destinations); err != nil {
				return &notificationHandlerError{fmt.Sprintf("Error in handleResendRequest. Error: %s\n", err)}
			}
		}
	}
	return nil
}

func handleAckResend() common.SyncServiceError {
	if trace.IsLogging(logger.TRACE) {
		trace.Trace("Handling ack resend objects\n")
	}
	common.ResendAcked = true
	return nil
}

func handleData(dataMessage []byte) common.SyncServiceError {
	orgID, objectType, objectID, dataReader, dataLength, offset, instanceID, err := parseDataMessage(dataMessage)
	if err != nil {
		return &notificationHandlerError{fmt.Sprintf("Error in handleData: failed to parse data. Error: %s\n", err.Error())}
	}

	if trace.IsLogging(logger.TRACE) {
		trace.Trace("Handling data of %s %s offset %d\n", objectType, objectID, offset)
	}

	metaData, err := Store.RetrieveObject(orgID, objectType, objectID)
	if err != nil || metaData == nil {
		return &notificationHandlerError{"Error in handleData: failed to find meta data.\n"}
	}

	lockIndex := common.HashStrings(orgID, objectType, objectID)
	lockObject(lockIndex)
	defer unlockObject(lockIndex)

	total, err := checkNotificationRecord(*metaData, metaData.OriginType, metaData.OriginID, instanceID,
		common.Getdata, offset)
	if err != nil {
		// This notification doesn't match the existing notification record, ignore
		if trace.IsLogging(logger.INFO) {
			trace.Info("Ignoring data of %s %s (%s)\n", objectType, objectID, err.Error())
		}
		return &notificationHandlerError{fmt.Sprintf("Error in handleData: checkNotificationRecord failed. Error: %s\n", err.Error())}
	}

	isFirstChunk := total == 0
	isLastChunk := total+int64(dataLength) >= metaData.ObjectSize

	if (offset != 0 || !isFirstChunk || !isLastChunk) && common.Configuration.NodeType == common.CSS && !leader.CheckIfLeader() {
		return &notificationHandlerError{"Only the leader node can handle chunked data"}
	}

	if dataLength != 0 {
		if metaData.DestinationDataURI != "" {
			if err := dataURI.AppendData(metaData.DestinationDataURI, dataReader, dataLength, offset, metaData.ObjectSize,
				isFirstChunk, isLastChunk); err != nil {
				return &notificationHandlerError{fmt.Sprintf("Error in handleData: failed to store data in data URI. Error: %s\n", err)}
			}
		} else {
			if err := Store.AppendObjectData(orgID, objectType, objectID, dataReader, dataLength, offset, metaData.ObjectSize,
				isFirstChunk, isLastChunk); err != nil {
				if storage.IsDiscarded(err) {
					return nil
				}
				return &notificationHandlerError{fmt.Sprintf("Error in handleData: failed to store data. Error: %s\n", err)}
			}
		}
	}

	maxRequestedOffset, err := handleChunkReceived(*metaData, offset, int64(dataLength))
	if err != nil {
		return &notificationHandlerError{"Error in handleData: handleChunkReceived failed. Error: " + err.Error()}
	}

	if isLastChunk {
		removeNotificationChunksInfo(*metaData, metaData.OriginType, metaData.OriginID)

		if err := Store.UpdateObjectStatus(orgID, objectType, objectID, common.CompletelyReceived); err != nil {
			return &notificationHandlerError{fmt.Sprintf("Error in handleData: %s\n", err)}
		}

		if err := SendObjectStatus(*metaData, common.Received); err != nil {
			return err
		}

		callWebhooks(metaData)

		return nil
	}

	newOffset := maxRequestedOffset + int64(metaData.ChunkSize)
	if newOffset < metaData.ObjectSize {
		// get next chunk
		if err := Comm.GetData(*metaData, newOffset); err != nil {
			return &notificationHandlerError{fmt.Sprintf("Error in handleData: failed to request data. Error: %s\n", err)}
		}
	}

	return nil
}

func handleGetData(metaData common.MetaData, offset int64) common.SyncServiceError {
	if trace.IsLogging(logger.TRACE) {
		trace.Trace("Handling data request for %s %s (offset %d)\n", metaData.ObjectType, metaData.ObjectID, offset)
	}

	notification, err := Store.RetrieveNotificationRecord(metaData.DestOrgID, metaData.ObjectType, metaData.ObjectID,
		metaData.DestType, metaData.DestID)
	if err != nil || notification == nil {
		return &notificationHandlerError{"Error in handleGetData: no notification to update."}
	}
	if notification.InstanceID != metaData.InstanceID ||
		(notification.Status != common.Update && notification.Status != common.Updated && notification.Status != common.Data) {
		// This notification doesn't match the existing notification record, ignore
		if trace.IsLogging(logger.TRACE) {
			trace.Trace("Ignoring get data request of %s %s\n", metaData.ObjectType, metaData.ObjectID)
		}
		return nil
	}

	var objectData []byte
	var length int
	var eof bool
	if metaData.SourceDataURI != "" {
		objectData, eof, length, err = dataURI.GetDataChunk(metaData.SourceDataURI, common.Configuration.MaxDataChunkSize,
			offset)
	} else {
		objectData, eof, length, err = Store.ReadObjectData(metaData.DestOrgID, metaData.ObjectType, metaData.ObjectID,
			common.Configuration.MaxDataChunkSize, offset)
	}
	if err != nil {
		return &notificationHandlerError{fmt.Sprintf("Error in handleGetData: failed to get object data. Error: %s\n", err)}
	}

	dataMessage, err := buildDataMessage(metaData, objectData, length, offset)
	if err != nil {
		return &notificationHandlerError{fmt.Sprintf("Error in handleGetData: failed to build data message. %s\n", err)}
	}

	chunked := false
	if offset != 0 || !eof {
		chunked = true
	}
	// Send data
	if err := Comm.SendData(metaData.DestOrgID, metaData.DestType, metaData.DestID, dataMessage, chunked); err != nil {
		return &notificationHandlerError{fmt.Sprintf("Error in handleGetData: failed to send notification. Error: %s\n", err)}
	}

	if err := Store.UpdateNotificationRecord(
		common.Notification{ObjectID: metaData.ObjectID, ObjectType: metaData.ObjectType,
			DestOrgID: metaData.DestOrgID, DestID: metaData.DestID, DestType: metaData.DestType,
			Status: common.Data, InstanceID: metaData.InstanceID},
	); err != nil {
		return &notificationHandlerError{fmt.Sprintf("Error in handleData: failed to update notification record. Error: %s\n", err)}
	}

	return nil
}

const (
	orgIDField = iota
	objectTypeField
	objectIDField
	offsetField
	dataField
	instanceIDField
	fieldCount
)

func buildDataMessage(metaData common.MetaData, data []byte, dataLength int, offset int64) ([]byte, common.SyncServiceError) {
	message := new(bytes.Buffer)

	// magic
	var value uint32 = common.Magic
	err := binary.Write(message, binary.BigEndian, value)
	if err != nil {
		return nil, &notificationHandlerError{"Failed to write magic to data message. Error: " + err.Error()}
	}

	// version
	value = common.Version
	err = binary.Write(message, binary.BigEndian, value)
	if err != nil {
		return nil, &notificationHandlerError{"Failed to write version to data message. Error: " + err.Error()}
	}

	// fieldCount
	value = fieldCount
	err = binary.Write(message, binary.BigEndian, value)
	if err != nil {
		return nil, &notificationHandlerError{"Failed to write field count to data message. Error: " + err.Error()}
	}

	// org id
	orgID := []byte(metaData.DestOrgID)

	// field type
	value = orgIDField
	err = binary.Write(message, binary.BigEndian, value)
	if err != nil {
		return nil, &notificationHandlerError{"Failed to write field type to data message. Error: " + err.Error()}
	}

	// length
	value = uint32(len(orgID))
	err = binary.Write(message, binary.BigEndian, value)
	if err != nil {
		return nil, &notificationHandlerError{"Failed to write field length to data message. Error: " + err.Error()}
	}

	// org ID data
	err = binary.Write(message, binary.BigEndian, orgID)
	if err != nil {
		return nil, &notificationHandlerError{"Failed to write org ID to data message. Error: " + err.Error()}
	}

	// object type
	objectType := []byte(metaData.ObjectType)

	// field type
	value = objectTypeField
	if err = binary.Write(message, binary.BigEndian, value); err != nil {
		return nil, &notificationHandlerError{"Failed to write field type to data message. Error: " + err.Error()}
	}

	// length
	value = uint32(len(objectType))
	if err = binary.Write(message, binary.BigEndian, value); err != nil {
		return nil, &notificationHandlerError{"Failed to write field length to data message. Error: " + err.Error()}
	}

	// type data
	if err = binary.Write(message, binary.BigEndian, objectType); err != nil {
		return nil, &notificationHandlerError{"Failed to write object type to data message. Error: " + err.Error()}
	}

	// object id
	objectID := []byte(metaData.ObjectID)

	// field type
	value = objectIDField
	if err = binary.Write(message, binary.BigEndian, value); err != nil {
		return nil, &notificationHandlerError{"Failed to write field type to data message. Error: " + err.Error()}
	}

	// length
	value = uint32(len(objectID))
	if err = binary.Write(message, binary.BigEndian, value); err != nil {
		return nil, &notificationHandlerError{"Failed to write field length to data message. Error: " + err.Error()}
	}

	// ID data
	if err = binary.Write(message, binary.BigEndian, objectID); err != nil {
		return nil, &notificationHandlerError{"Failed to write object ID to data message. Error: " + err.Error()}
	}

	// offset
	// field type
	value = offsetField
	if err = binary.Write(message, binary.BigEndian, value); err != nil {
		return nil, &notificationHandlerError{"Failed to write field type to data message. Error: " + err.Error()}
	}

	// offset length
	value = uint32(binary.Size(offset))
	if err = binary.Write(message, binary.BigEndian, value); err != nil {
		return nil, &notificationHandlerError{"Failed to write offset length to data message. Error: " + err.Error()}
	}

	// offset
	if err = binary.Write(message, binary.BigEndian, offset); err != nil {
		return nil, &notificationHandlerError{"Failed to write offset to data message. Error: " + err.Error()}
	}

	// instance ID
	// field type
	value = instanceIDField
	if err = binary.Write(message, binary.BigEndian, value); err != nil {
		return nil, &notificationHandlerError{"Failed to write field type to data message. Error: " + err.Error()}
	}

	// instance ID length
	value = uint32(binary.Size(metaData.InstanceID))
	if err = binary.Write(message, binary.BigEndian, value); err != nil {
		return nil, &notificationHandlerError{"Failed to write instance ID length to data message. Error: " + err.Error()}
	}

	// instance ID
	if err = binary.Write(message, binary.BigEndian, metaData.InstanceID); err != nil {
		return nil, &notificationHandlerError{"Failed to write instance ID to data message. Error: " + err.Error()}
	}

	// field type
	value = dataField
	if err = binary.Write(message, binary.BigEndian, value); err != nil {
		return nil, &notificationHandlerError{"Failed to write field type to data message. Error: " + err.Error()}
	}

	// data length
	value = uint32(dataLength)
	if err = binary.Write(message, binary.BigEndian, value); err != nil {
		return nil, &notificationHandlerError{"Failed to write data length to data message. Error: " + err.Error()}
	}

	// data
	if dataLength != 0 {
		err = binary.Write(message, binary.BigEndian, data)
		if err != nil {
			return nil, &notificationHandlerError{"Failed to write data to data message. Error: " + err.Error()}
		}
	}

	return message.Bytes(), nil
}

func parseDataMessage(message []byte) (orgID string, objectType string, objectID string, dataReader io.Reader, dataLength uint32,
	offset int64, instanceID int64, err common.SyncServiceError) {
	var (
		magicValue  uint32
		version     uint32
		fieldCount  uint32
		fieldType   uint32
		fieldLength uint32
		rawString   []byte
		count       int
		dataOffset  int64
	)

	messageReader := bytes.NewReader(message)
	if err = binary.Read(messageReader, binary.BigEndian, &magicValue); err != nil {
		return
	}
	if magicValue != common.Magic {
		err = &notificationHandlerError{"Invalid data."}
		return
	}

	if err = binary.Read(messageReader, binary.BigEndian, &version); err != nil {
		return
	}
	if version != common.Version {
		err = &notificationHandlerError{"Wrong data version."}
		return
	}

	if err = binary.Read(messageReader, binary.BigEndian, &fieldCount); err != nil {
		return
	}

	for i := 0; i < int(fieldCount); i++ {
		if err = binary.Read(messageReader, binary.BigEndian, &fieldType); err != nil {
			return
		}
		if err = binary.Read(messageReader, binary.BigEndian, &fieldLength); err != nil {
			return
		}

		switch int(fieldType) {
		case objectTypeField:
			rawString = make([]byte, fieldLength)
			count, err = messageReader.Read(rawString)
			if err != nil {
				return
			}
			if count != int(fieldLength) {
				err = &notificationHandlerError{fmt.Sprintf("Read %d bytes for the object type, instead of %d", count, fieldLength)}
				return
			}
			objectType = string(rawString)

		case orgIDField:
			rawString = make([]byte, fieldLength)
			count, err = messageReader.Read(rawString)
			if err != nil {
				return
			}
			if count != int(fieldLength) {
				err = &notificationHandlerError{fmt.Sprintf("Read %d bytes for the org ID, instead of %d", count, fieldLength)}
				return
			}
			orgID = string(rawString)

		case objectIDField:
			rawString = make([]byte, fieldLength)
			count, err = messageReader.Read(rawString)
			if err != nil {
				return
			}
			if count != int(fieldLength) {
				err = &notificationHandlerError{fmt.Sprintf("Read %d bytes for the object id, instead of %d", count, fieldLength)}
				return
			}
			objectID = string(rawString)

		case offsetField:
			if fieldLength != uint32(binary.Size(offset)) {
				err = &notificationHandlerError{fmt.Sprintf("Length field for offset wasn't %d, it was %d", uint32(binary.Size(offset)),
					fieldLength)}
				return
			}
			if err = binary.Read(messageReader, binary.BigEndian, &offset); err != nil {
				return
			}

		case instanceIDField:
			if fieldLength != uint32(binary.Size(instanceID)) {
				err = &notificationHandlerError{fmt.Sprintf("Length field for instance ID wasn't %d, it was %d", uint32(binary.Size(instanceID)),
					fieldLength)}
				return
			}
			if err = binary.Read(messageReader, binary.BigEndian, &instanceID); err != nil {
				return
			}

		case dataField:
			dataLength = fieldLength
			dataOffset, err = messageReader.Seek(0, os.SEEK_CUR)
			if err != nil {
				return
			}
		default:
			if trace.IsLogging(logger.TRACE) {
				trace.Trace("parseDataMessage encoutered an unrecognized field of type: %d, the Type/Length/Value is ignored\n", fieldType)
			}
			messageReader.Seek(int64(fieldLength), os.SEEK_CUR)
		}
	}

	if objectType == "" || objectID == "" || dataOffset == 0 {
		err = &notificationHandlerError{"Invalid data message\n"}
		return
	}

	_, err = messageReader.Seek(dataOffset, os.SEEK_SET)
	if err != nil {
		return
	}

	dataReader = io.LimitReader(messageReader, int64(dataLength))
	return
}

// checkNotificationRecord checks notification's instanceID, status and offset.
// It returns the expected size of the data and no error if everything is OK, and 0 and an error if not.
func checkNotificationRecord(metaData common.MetaData, destType string, destID string, instanceID int64,
	status string, offset int64) (int64, common.SyncServiceError) {

	notification, err := Store.RetrieveNotificationRecord(metaData.DestOrgID, metaData.ObjectType, metaData.ObjectID,
		destType, destID)
	if err != nil {
		return 0, err
	}
	if notification == nil {
		return 0, &notificationHandlerError{"No notification"}
	}

	if notification.InstanceID != instanceID {
		return 0, &notificationHandlerError{fmt.Sprintf("InstanceID mismatch: expected=%d, received=%d", notification.InstanceID, instanceID)}
	}
	if notification.Status != status {
		return 0, &notificationHandlerError{fmt.Sprintf("Status mismatch: expected=%s, received=%s", notification.Status, status)}
	}
	id := common.CreateNotificationID(metaData.DestOrgID, metaData.ObjectType, metaData.ObjectID, destType, destID)
	notificationLock.RLock()
	chunksInfo, ok := notificationChunks[id]
	notificationLock.RUnlock()
	if !ok {
		return 0, &notificationHandlerError{"No notification chunk info"}
	}
	if _, ok := chunksInfo.chunkResendTimes[offset]; !ok {
		return 0, &notificationHandlerError{fmt.Sprintf("Offset mismatch: %d not found in set of inflight requests", offset)}
	}
	if len(chunksInfo.chunksReceived) == 0 {
		return 0, &notificationHandlerError{"Invalid chunks info"}
	}
	return chunksInfo.receivedDataSize, nil
}

func updateGetDataNotification(metaData common.MetaData, destType string, destID string, offset int64) common.SyncServiceError {
	return updateNotificationChunkInfo(true, metaData, destType, destID, offset)
}

// Can be only called after obtaining a notification lock
func updateNotificationChunkInfo(createNotification bool, metaData common.MetaData, destType string, destID string, offset int64) common.SyncServiceError {
	id := common.CreateNotificationID(metaData.DestOrgID, metaData.ObjectType, metaData.ObjectID, destType, destID)
	notificationLock.RLock()
	chunksInfo, ok := notificationChunks[id]
	notificationLock.RUnlock()

	if !ok {
		if createNotification {
			if err := Store.UpdateNotificationRecord(
				common.Notification{ObjectID: metaData.ObjectID, ObjectType: metaData.ObjectType,
					DestOrgID: metaData.DestOrgID, DestID: destID, DestType: destType,
					Status: common.Getdata, InstanceID: metaData.InstanceID},
			); err != nil {
				return &notificationHandlerError{fmt.Sprintf("Failed to update notification record. Error: %s\n", err)}
			}
		}

		chunksInfo = notificationChunksInfo{chunkSize: metaData.ChunkSize, chunkResendTimes: make(map[int64]int64)}
		if chunksInfo.chunkSize > 0 {
			numberOfBytes := int(((metaData.ObjectSize/int64(chunksInfo.chunkSize) + 1) / 8) + 1)
			chunksInfo.chunksReceived = make([]byte, numberOfBytes)
		}
	}

	resendTime := time.Now().Unix() + int64(common.Configuration.ResendInterval*6)
	chunksInfo.chunkResendTimes[offset] = resendTime

	if chunksInfo.maxRequestedOffset < offset {
		chunksInfo.maxRequestedOffset = offset
	}

	chunksInfo.resendTime = resendTime
	notificationLock.Lock()
	notificationChunks[id] = chunksInfo
	notificationLock.Unlock()
	return nil
}

func removeNotificationChunksInfo(metaData common.MetaData, destType string, destID string) {
	id := common.CreateNotificationID(metaData.DestOrgID, metaData.ObjectType, metaData.ObjectID, destType, destID)
	notificationLock.Lock()
	delete(notificationChunks, id)
	notificationLock.Unlock()
}

func handleChunkReceived(metaData common.MetaData, offset int64, size int64) (int64, common.SyncServiceError) {
	id := common.CreateNotificationID(metaData.DestOrgID, metaData.ObjectType, metaData.ObjectID, metaData.OriginType, metaData.OriginID)
	notificationLock.RLock()
	chunksInfo, ok := notificationChunks[id]
	notificationLock.RUnlock()
	if !ok {
		return 0, &notificationHandlerError{"Chunks info not found"}
	}

	if _, ok := chunksInfo.chunkResendTimes[offset]; !ok {
		return 0, &notificationHandlerError{"Chunk's resend time not found"}
	}
	delete(chunksInfo.chunkResendTimes, offset)

	// The chunksInfo.chunksReceived byte array holds a bit per chunk (identified by its offset), so each byte holds the bits of 8 chunks.
	// To access the bit of a given chunk:
	//  offset/chunkSize is the chunkIndex
	//  chunkIndex/8 is the byteIndex
	//  chunkIndex&7 is the bitIndex
	//  (1 << bitIndex) is the bitMask which has 1 at bitIndex
	chunkIndex := uint(offset / int64(chunksInfo.chunkSize))
	byteIndex := chunkIndex >> 3
	bitIndex := chunkIndex & 7
	bitMask := byte(1 << bitIndex)
	if chunksInfo.chunksReceived[byteIndex]&bitMask == 0 {
		chunksInfo.receivedDataSize += size
		chunksInfo.chunksReceived[byteIndex] |= bitMask
	} else {
		if trace.IsLogging(logger.INFO) {
			trace.Info("Chunk with offset %d of object %s:%s:%s already received.\n", offset,
				metaData.DestOrgID, metaData.ObjectType, metaData.ObjectID)
		}
	}

	if chunksInfo.maxReceivedOffset < offset {
		chunksInfo.maxReceivedOffset = offset
	}

	chunksInfo.resendTime = time.Now().Unix() + int64(common.Configuration.ResendInterval*6)
	notificationLock.Lock()
	notificationChunks[id] = chunksInfo
	notificationLock.Unlock()

	return chunksInfo.maxRequestedOffset, nil
}

func handleDataReceived(metaData common.MetaData) {
	removeNotificationChunksInfo(metaData, metaData.OriginType, metaData.OriginID)
}

func getOffsetsToResend(notification common.Notification, metaData common.MetaData) []int64 {
	offsets := make([]int64, 0)

	id := common.GetNotificationID(notification)
	notificationLock.RLock()
	chunksInfo, ok := notificationChunks[id]
	notificationLock.RUnlock()
	if !ok {
		return getOffsetsForResendFromScratch(notification, metaData)
	}

	// The code below checks for missing chunks, i.e., chunks that have been requested but not received (e.g., lost in the network)
	// We do this when chunksInfo.resendTime is less than the current time, meaning no chunks were received during that period.
	// Otherwise, we may still want to ask to resend chunks, if they were received out of order.
	// To check this efficiently (without scanning the map each time) the code maintains two parameters:
	//  1. maxRequestedOffset - the largest offset that has been requested (updated when a new offset is requested)
	//  2. maxReceivedOffset - the largest offset that has been received (updated when a new chunk with a larger offset is received)
	// Information about the chunks that have been requested but not yet received is stored in the chunksInfo.chunkResendTimes.
	// When the chunks arrive without any gaps (maxRequestedOffset-maxReceivedOffset)/chunkSize should be equal to the number of
	// elements in the chunkResendTimes map.
	// If the number of elements in the map, i.e. len(chunksInfo.chunkResendTimes), is larger, it means that one or more chunks has not
	// been received or that chunks have been received out of order.
	// In such cases we want to scan the map and see if a chunk has to be re-requested.
	currentTime := time.Now().Unix()
	if chunksInfo.resendTime <= currentTime ||
		(chunksInfo.chunkSize > 0 &&
			int(chunksInfo.maxRequestedOffset-chunksInfo.maxReceivedOffset)/chunksInfo.chunkSize < len(chunksInfo.chunkResendTimes)) {
		for offset, resendTime := range chunksInfo.chunkResendTimes {
			if resendTime <= currentTime {
				offsets = append(offsets, offset)
			}
		}
	}
	return offsets
}

// Handle the case of resending get data notification after node restart, i.e. there is no chunksInfo for the notification
// Can be only called after obtaining a notification lock
func getOffsetsForResendFromScratch(notification common.Notification, metaData common.MetaData) []int64 {
	offsets := make([]int64, 0)

	protocol, err := Store.RetrieveDestinationProtocol(notification.DestOrgID, notification.DestType, notification.DestID)
	if err != nil {
		if log.IsLogging(logger.ERROR) {
			log.Error("Failed to resend getdata notification. Error: %s\n", err)
		}
		return offsets
	}

	maxInflightChunks := 1
	if protocol == common.MQTTProtocol {
		maxInflightChunks = common.Configuration.MaxInflightChunks
	}

	if err := updateNotificationChunkInfo(false, metaData, notification.DestType, notification.DestID, 0); err != nil {
		if log.IsLogging(logger.ERROR) {
			log.Error("Failed to resend getdata notification. Error: %s\n", err)
		}

		return offsets
	}

	if metaData.ChunkSize <= 0 || metaData.ObjectSize <= 0 {
		offsets = append(offsets, 0)
	} else {
		var offset int64
		for i := 0; i < maxInflightChunks && offset < metaData.ObjectSize; i++ {
			offsets = append(offsets, offset)
			offset += int64(metaData.ChunkSize)
		}
	}
	return offsets
}