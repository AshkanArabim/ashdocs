import { useEffect, useState, useRef } from 'react'
import './App.css'
import * as Automerge from "@automerge/automerge";

interface Doc {
  text: string;
}

// Helper function to get document ID from URL
function getDocumentIdFromUrl(): string | null {
  const params = new URLSearchParams(window.location.search);
  return params.get('doc');
}

// Helper function to generate a new document ID
function generateDocumentId(): string {
  return Math.random().toString(36).substring(2, 15) + Math.random().toString(36).substring(2, 15);
}

// Helper function to update URL with document ID
function updateUrlWithDocumentId(docId: string) {
  const url = new URL(window.location.href);
  url.searchParams.set('doc', docId);
  window.history.replaceState({}, '', url.toString());
}

function App() {
  const [text, setText] = useState("");
  const docRef = useRef<Automerge.Doc<Doc> | null>(null);
  const syncStateRef = useRef<Automerge.SyncState | null>(null);
  const wsRef = useRef<WebSocket | null>(null);
  const sendSyncMessageRef = useRef<(() => void) | null>(null);

  // Initialize document and websocket connection
  useEffect(() => {
    let isMounted = true;

    // Check if document ID is in URL
    let urlDocId = getDocumentIdFromUrl();
    
    if (!urlDocId) {
      // Generate a new document ID
      urlDocId = generateDocumentId();
      updateUrlWithDocumentId(urlDocId);
    }
    
    // Initialize Automerge document
    let doc = Automerge.init<Doc>();
    const syncState = Automerge.initSyncState();
    docRef.current = doc;
    syncStateRef.current = syncState;

    // Connect to backend websocket
    const wsUrl = `ws://localhost:8080/document/${urlDocId}`;
    console.log('Connecting to WebSocket:', wsUrl);
    const ws = new WebSocket(wsUrl);
    wsRef.current = ws;

    ws.binaryType = 'arraybuffer';

    function sendSyncMessage() {
      if (!docRef.current || !syncStateRef.current || !isMounted) {
        console.log('sendSyncMessage: skipping - refs not ready');
        return;
      }

      if (ws.readyState !== WebSocket.OPEN) {
        console.log('sendSyncMessage: skipping - WebSocket not open', ws.readyState);
        return;
      }

      const [newSyncState, syncMessage] = Automerge.generateSyncMessage(
        docRef.current,
        syncStateRef.current
      );

      syncStateRef.current = newSyncState;

      if (syncMessage) {
        console.log('sendSyncMessage: sending message', syncMessage.byteLength, 'bytes');
        // Send as binary
        ws.send(syncMessage);
      } else {
        console.log('sendSyncMessage: no message to send (already in sync)');
      }
    }

    function receiveSyncMessage(message: Uint8Array) {
      if (!docRef.current || !syncStateRef.current || !isMounted) {
        return;
      }

      console.log('receiveSyncMessage: processing message', message.byteLength, 'bytes');

      const [newDoc, newSyncState] = Automerge.receiveSyncMessage(
        docRef.current,
        syncStateRef.current,
        message
      );

      docRef.current = newDoc;
      syncStateRef.current = newSyncState;

      // Update UI if text changed
      if (isMounted && newDoc.text !== undefined) {
        const oldText = text;
        const newText = newDoc.text || "";
        if (oldText !== newText) {
          console.log('receiveSyncMessage: text changed from', oldText.length, 'to', newText.length, 'chars');
          setText(newText);
        }
      }

      // Send a sync message back if needed
      sendSyncMessage();
    }

    ws.onopen = () => {
      console.log('WebSocket connected');
      // nothing to do here. server will send initial document content when connection opens
    };

    ws.onmessage = (event) => {
      if (event.data instanceof ArrayBuffer) {
        // Convert ArrayBuffer to Uint8Array
        const message = new Uint8Array(event.data);
        receiveSyncMessage(message);
      } else {
        console.warn('Received non-ArrayBuffer message:', event.data);
      }
    };

    ws.onerror = (error) => {
      console.error('WebSocket error:', error);
    };

    ws.onclose = (event) => {
      console.log('WebSocket disconnected', {
        code: event.code,
        reason: event.reason,
        wasClean: event.wasClean
      });
    };

    // Store sendSyncMessage function for use in handleChange
    sendSyncMessageRef.current = sendSyncMessage;

    return () => {
      console.log('Cleaning up WebSocket connection');
      isMounted = false;
      if (ws && ws.readyState === WebSocket.OPEN || ws.readyState === WebSocket.CONNECTING) {
        ws.close(1000, 'Component unmounting');
      }
      sendSyncMessageRef.current = null;
    };
  }, []);

  const handleChange = (e: React.ChangeEvent<HTMLTextAreaElement>) => {
    const newText = e.target.value;
    console.log('handleChange: user typed, new length:', newText.length);
    setText(newText);
    
    // Update the Automerge document
    if (docRef.current && syncStateRef.current) {
      const oldDoc = docRef.current;
      docRef.current = Automerge.change(docRef.current, (doc: Doc) => {
        doc.text = newText;
      });
      console.log('handleChange: document updated, changed:', oldDoc !== docRef.current);

      // DO NOT reset the sync state! The sync state tracks what the server knows.
      // generateSyncMessage will automatically detect that our document has changed
      // and include the new changes in the sync message.

      // Trigger sync message send
      if (sendSyncMessageRef.current) {
        console.log('handleChange: triggering sync');
        sendSyncMessageRef.current();
      } else {
        console.warn('handleChange: sendSyncMessageRef is null!');
      }
    }
  };

  return (
    <textarea
      value={text}
      onChange={handleChange}
      style={{
        width: "100vw",
        height: "100vh",
        boxSizing: "border-box",
        resize: "none",
        border: "none",
        outline: "none",
      }}
      placeholder="Start typing... (synced with Automerge)"
      autoFocus
    />
  );
}

export default App
