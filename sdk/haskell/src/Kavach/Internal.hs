-- | The restricted monad and the types shared by the recorder and the host.
-- Not exported from the package: the 'Kavach' constructor and 'unsafeIO' are
-- the escape hatch that lets the SDK itself (and only it) run IO.
module Kavach.Internal
  ( Kavach (..)
  , EnvImpl (..)
  , Scope (..)
  , Input (..)
  , Output (..)
  , GatewayError (..)
  , Handler (..)
  , Failure (..)
  , HandlerFailure (..)
  , KavachAbort (..)
  , unsafeIO
  , classify
  , failureMessage
  , runStep
  , checkInvariants
  , producer
  , runtimeName
  ) where

import Control.Exception hiding (Handler, handle)
import Data.ByteString (ByteString)
import Data.Char (isHexDigit)
import Data.Dynamic (Dynamic)
import Data.Int (Int64)
import Data.List (isInfixOf, isPrefixOf)
import Data.Text (Text)
import qualified Data.Text as T
import Data.Version (showVersion)
import System.Info (compilerName, compilerVersion)

-- | Everything a handler may do that touches the world. Its only IO comes from
-- 'EnvImpl', so what it reads is what the SDK records or replays.
newtype Kavach a = Kavach {unKavach :: EnvImpl -> IO a}

instance Functor Kavach where
  fmap f (Kavach g) = Kavach (fmap f . g)

instance Applicative Kavach where
  pure x = Kavach (\_ -> pure x)
  Kavach f <*> Kavach g = Kavach (\e -> f e <*> g e)

instance Monad Kavach where
  Kavach g >>= f = Kavach (\e -> g e >>= \a -> unKavach (f a) e)

unsafeIO :: IO a -> Kavach a
unsafeIO io = Kavach (const io)

data Scope = Remote | Local deriving (Eq, Show)

data EnvImpl = EnvImpl
  { envClock :: IO Int64
  , envRandom :: Int -> IO ByteString
  , envQuery :: Text -> ByteString -> IO (Either GatewayError ByteString)
  , envConfig :: Text -> IO (Maybe ByteString)
  , envEmit :: Scope -> Text -> ByteString -> IO ()
  , envCommit :: Dynamic -> IO ()
  }

-- | One event consumed by a handler.
data Input = Input
  { inputSource :: Text
  , inputPosition :: Text
  , inputData :: ByteString
  }
  deriving (Eq, Show)

-- | One effect a handler requested with 'Kavach.emit'.
data Output = Output
  { outputSink :: Text
  , outputData :: ByteString
  , outputScope :: Scope
  }
  deriving (Eq, Show)

-- | A gateway query failed; the text is what the connection reported.
newtype GatewayError = GatewayError Text
  deriving (Eq, Show)

instance Exception GatewayError

-- | A handler: explicit state, a step function in the restricted monad,
-- and optional snapshot support and invariants.
data Handler s = Handler
  { handle :: Input -> s -> Kavach s
  , initial :: s
  , snapshot :: Maybe (s -> ByteString)
  , restore :: Maybe (ByteString -> Either String s)
  , invariants :: [(Text, s -> Either Text ())]
  }

data Failure = Failure
  { failKind :: Text -- ^ "panic", "error" or "invariant"
  , failMessage :: Text
  , failDetail :: Text
  }
  deriving (Eq, Show)

data HandlerFailure = HandlerPanic Text | HandlerError Text deriving (Show)

instance Exception HandlerFailure where
  displayException (HandlerPanic m) = T.unpack m
  displayException (HandlerError m) = T.unpack m

-- | Unwinds a step after the driver answered a request with @abort@ (SPEC §9.4).
-- Handlers cannot catch it: 'Kavach' has no @catch@.
data KavachAbort = KavachAbort deriving (Show)

instance Exception KavachAbort

producer :: String
producer = "kavach-haskell/0.1.0"

-- | The @host.runtime@ fact, e.g. @ghc-9.14.1@.
runtimeName :: String
runtimeName = compilerName ++ "-" ++ showVersion compilerVersion

-- | The failure mapping (README, SPEC §4.5): 'kavachPanic' and 'kavachError'
-- give exactly their message; any other exception is a panic whose message is
-- 'failureMessage' of it and whose detail is the full 'displayException'.
classify :: SomeException -> Failure
classify e = case fromException e of
  Just (HandlerPanic m) -> Failure "panic" m detail
  Just (HandlerError m) -> Failure "error" m detail
  Nothing -> Failure "panic" (failureMessage e) detail
  where
    detail = T.pack (displayException e)

-- | The location-independent message of an exception (SPEC §4.5): GHC puts
-- call stacks, source spans and addresses into messages; those stay in the
-- detail, not the message.
failureMessage :: SomeException -> Text
failureMessage e =
  T.strip . T.unwords . filter (not . T.null) . map cleanLine . takeWhile (not . stackStart) $ lines (displayException e)
  where
    stackStart l = any (`isPrefixOf` l) ["CallStack (from HasCallStack)", "HasCallStack backtrace", "Cabal backtrace"]
    cleanLine = T.unwords . filter (not . T.null) . map T.pack . filter keep . words
    keep w = not (".hs:" `isInfixOf` w || address w)
    address w = case w of
      '0' : 'x' : h@(_ : _) -> all isHexDigit h
      _ -> False

-- | Run one handler step: the new state (to WHNF), or the failure and the
-- exception behind it.
runStep :: Handler s -> EnvImpl -> Input -> s -> IO (Either (Failure, SomeException) s)
runStep h env inp s0 = do
  r <- try (unKavach (handle h inp s0) env >>= evaluate)
  pure $ either (\e -> Left (classify e, e)) Right r

-- | The first invariant that fails, in declaration order. A check that throws
-- fails too.
checkInvariants :: Handler s -> s -> IO (Maybe Failure)
checkInvariants h s = go (invariants h)
  where
    go [] = pure Nothing
    go ((name, chk) : rest) = do
      r <- try (evaluate (chk s))
      case r of
        Right (Right ()) -> go rest
        Right (Left why) -> pure (Just (Failure "invariant" name why))
        Left ex -> pure (Just (Failure "invariant" name (T.pack (displayException (ex :: SomeException)))))
