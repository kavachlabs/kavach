-- | Kavach for Haskell: handlers run in the restricted 'Kavach' monad, whose
-- only effects are the clock, randomness, gateway queries, config reads and
-- emitting outputs. There is no way to reach @getCurrentTime@ or @randomIO@
-- from a handler, so the type checker enforces what the other SDKs ask for
-- by convention (SPEC §1, §6).
module Kavach
  ( -- * Handlers
    Handler (..)
  , Input (..)
  , Output (..)
  , Scope (..)
  , Kavach
    -- * Effects
  , now
  , nowNanos
  , random
  , query
  , GatewayError (..)
  , config
  , emit
  , emitLocal
  , commit
  , kavachError
  , kavachPanic
    -- * Failures
  , Failure (..)
  , failureMessage
    -- * Recording
  , RecorderOptions (..)
  , Start (..)
  , Gateway (..)
  , Recorder
  , StepResult (..)
  , RecorderError (..)
  , defaultOptions
  , newRecorder
  , withRecorder
  , step
  , flush
  , closeRecorder
  , stepOk
    -- * Replay
  , maybeHost
  ) where

import Kavach.Effects
import Kavach.Host
import Kavach.Internal
import Kavach.Recorder
