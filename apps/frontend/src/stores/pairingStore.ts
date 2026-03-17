// Copyright (c) OpenLobster contributors. See LICENSE for details.

import { createSignal } from "solid-js";
import type { PairingRequestEvent } from "@openlobster/ui/hooks";

const [pendingPairingsQueue, setPendingPairingsQueue] = createSignal<PairingRequestEvent[]>([]);

export { pendingPairingsQueue, setPendingPairingsQueue };
