-- | The effects a handler has: the whole of them.
module Kavach.Effects
  ( now
  , nowNanos
  , random
  , query
  , config
  , emit
  , emitLocal
  , commit
  , kavachError
  , kavachPanic
  ) where

import Control.Exception (throwIO)
import Data.ByteString (ByteString)
import qualified Data.ByteString as BS
import Data.Dynamic (Typeable, toDyn)
import Data.Int (Int64)
import Data.Text (Text)
import Data.Time.Clock (UTCTime)
import Data.Time.Clock.POSIX (posixSecondsToUTCTime)
import Kavach.Internal

-- | The clock, in nanoseconds since the Unix epoch (UTC). Recorded live,
-- served from the journal in replay.
nowNanos :: Kavach Int64
nowNanos = Kavach envClock

-- | The clock as a time (microsecond precision).
now :: Kavach UTCTime
now = fmap (\ns -> posixSecondsToUTCTime (fromIntegral (ns `div` 1000) / 1000000)) nowNanos

-- | @n@ random bytes. @n <= 0@ reads nothing.
random :: Int -> Kavach ByteString
random n
  | n <= 0 = pure BS.empty
  | otherwise = Kavach (\e -> envRandom e n)

-- | Query an external system through a registered gateway. A failed query is
-- a value, because the failure is part of the recording.
query :: Text -> ByteString -> Kavach (Either GatewayError ByteString)
query gateway request = Kavach (\e -> envQuery e gateway request)

-- | A config value that can change what the handler does, such as a flag.
config :: Text -> Kavach (Maybe ByteString)
config key = Kavach (\e -> envConfig e key)

-- | Request an effect on a remote system. Delivered only after the step
-- succeeds when recording; compared, never delivered, in replay.
emit :: Text -> ByteString -> Kavach ()
emit sink d = Kavach (\e -> envEmit e Remote sink d)

-- | Like 'emit', for a resource of the host the process runs on (SPEC §6.3).
emitLocal :: Text -> ByteString -> Kavach ()
emitLocal sink d = Kavach (\e -> envEmit e Local sink d)

-- | Keep this state even if the step fails later. Without it a failed step
-- leaves the state as it was when the step began. Use it for handlers whose
-- partial progress is real (they have already queried or emitted).
commit :: Typeable s => s -> Kavach ()
commit s = Kavach (\e -> envCommit e (toDyn s))

-- | Fail the step as a @panic@ whose marker message is exactly this text.
kavachPanic :: Text -> Kavach a
kavachPanic = Kavach . const . throwIO . HandlerPanic

-- | Fail the step as an @error@ whose marker message is exactly this text.
kavachError :: Text -> Kavach a
kavachError = Kavach . const . throwIO . HandlerError
