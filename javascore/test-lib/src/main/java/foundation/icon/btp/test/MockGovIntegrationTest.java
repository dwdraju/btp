/*
 * Copyright 2022 ICON Foundation
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package foundation.icon.btp.test;

import foundation.icon.btp.mock.ChainScore;
import foundation.icon.btp.mock.ChainScoreClient;
import foundation.icon.btp.mock.MockGov;
import foundation.icon.btp.mock.MockGovScoreClient;
import foundation.icon.icx.IconService;
import foundation.icon.icx.KeyWallet;
import foundation.icon.icx.data.Base64;
import foundation.icon.icx.transport.http.HttpProvider;
import foundation.icon.jsonrpc.Address;
import foundation.icon.score.client.DefaultScoreClient;
import foundation.icon.score.util.StringUtil;
import org.junit.jupiter.api.Tag;

import java.io.IOException;
import java.math.BigInteger;
import java.util.List;
import java.util.concurrent.atomic.AtomicLong;

import static org.junit.jupiter.api.Assertions.assertEquals;

@Tag("integration")
public interface MockGovIntegrationTest {

    MockGovScoreClient mockGovClient = new MockGovScoreClient(
            DefaultScoreClient.of("gov-mock.", System.getProperties()));
    MockGov mockGov = mockGovClient;
    KeyWallet validatorWallet = (KeyWallet) DefaultScoreClient.wallet("validator.", System.getProperties());
    ChainScoreClient chainScoreClient = new ChainScoreClient(mockGovClient.endpoint(), mockGovClient._nid(), validatorWallet,
            new Address(ChainScore.ADDRESS.toString()));
    ChainScore chainScore = chainScoreClient;
    IconService iconService = new IconService(new HttpProvider(mockGovClient.endpoint()));

    static long openBTPNetwork(String networkTypeName, String name, score.Address owner) {
        ensureRevision();
        ensureBTPPublicKey();
        AtomicLong networkId = new AtomicLong();
        mockGovClient.openBTPNetwork((txr) -> {
                    List<BTPNetworkOpenedEventLog> l = BTPNetworkOpenedEventLog.eventLogs(txr);
                    networkId.set(l.get(0).getNetworkId());
                }
                , networkTypeName, name, owner);
        return networkId.get();
    }

    static void closeBTPNetwork(long networkId) {
        mockGovClient.closeBTPNetwork((txr) -> {
                    List<BTPNetworkClosedEventLog> l = BTPNetworkClosedEventLog.eventLogs(txr);
                    assertEquals(networkId, l.get(0).getNetworkId());
                }
                , networkId);
    }

    static void ensureRevision() {
        final int revision = 9;
        if (revision != chainScore.getRevision()) {
            mockGov.setRevision(revision);
        }
    }

    static void ensureBTPPublicKey() {
        String DSA = "ecdsa/secp256k1";
        Address address = Address.of(validatorWallet);
        byte[] pubKey = chainScore.getBTPPublicKey(address, DSA);
        System.out.println("getPublicKey:" + StringUtil.bytesToHex(pubKey));
        if (pubKey == null) {
            pubKey = validatorWallet.getPublicKey().toByteArray();
            System.out.println("setBTPPublicKey:" + StringUtil.bytesToHex(pubKey));
            chainScore.setBTPPublicKey(DSA, pubKey);
        }
    }

    static byte[][] getMessages(long height, long networkId) {
        try {
            Base64[] base64Messages = iconService.btpGetMessages(
                    BigInteger.valueOf(height),
                    BigInteger.valueOf(networkId)).execute();
            byte[][] messages = new byte[base64Messages.length][];
            for (int i = 0; i < base64Messages.length; i++) {
                messages[i] = base64Messages[i].decode();
            }
            return messages;
        } catch (IOException e) {
            throw new RuntimeException(e);
        }

    }
}
