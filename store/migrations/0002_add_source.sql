-- source is the channel a note arrived through, such as "index" for the device
-- webhook or "web" for the web interface. transcribed_by names what produced the
-- text: "index" when the device sent it, a transcription model when Riverbed
-- made it, and NULL when the note was typed.

ALTER TABLE recordings ADD COLUMN source TEXT NOT NULL DEFAULT 'index';
ALTER TABLE recordings ADD COLUMN transcribed_by TEXT;

UPDATE recordings SET source = 'web' WHERE client = 'web';
UPDATE recordings SET transcribed_by = 'index'
 WHERE source = 'index' AND transcription IS NOT NULL;
