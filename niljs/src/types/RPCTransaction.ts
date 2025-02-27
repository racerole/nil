import type { Hex } from "./Hex.js";

export type Flags = "Internal" | "External" | "Deploy" | "Refund" | "Bounce" | "Response";

/**
 * The structure representing a JSON-RPC transaction.
 *
 * @export
 * @typedef {RPCTransaction}
 */
export type RPCTransaction = {
  flags: Flags[];
  success: boolean;
  data: Hex;
  blockHash: Hex;
  blockNumber: number;
  from: Hex;
  gasUsed: Hex;
  feeCredit: string;
  maxPriorityFeePerGas: string;
  maxFeePerGas: string;
  hash: Hex;
  seqno: Hex;
  to: Hex;
  refundTo: Hex;
  bounceTo: Hex;
  index?: Hex;
  value: string;
  signature: Hex;
};
