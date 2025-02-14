// SPDX-License-Identifier: Apache-2.0
pragma solidity >=0.8.0 <0.8.5;
pragma abicoder v2;

import "../interfaces/IBSH.sol";
import "../interfaces/IBMV.sol";
import "../interfaces/IBMCPeriphery.sol";
import "../interfaces/IBMCManagement.sol";
import "../libraries/ParseAddress.sol";
import "../libraries/RLPDecodeStruct.sol";
import "../libraries/RLPEncodeStruct.sol";
import "../libraries/String.sol";
import "../libraries/Types.sol";

import "@openzeppelin/contracts-upgradeable/proxy/utils/Initializable.sol";

contract BMCPeripheryV2 is IBMCPeriphery, Initializable {
    using String for string;
    using ParseAddress for address;
    using RLPDecodeStruct for bytes;
    using RLPEncodeStruct for Types.BTPMessage;
    using RLPEncodeStruct for Types.ErrorMessage;

    uint256 internal constant UNKNOWN_ERR = 0;
    uint256 internal constant BMC_ERR = 10;
    uint256 internal constant BMV_ERR = 25;
    uint256 internal constant BSH_ERR = 40;

    string internal constant BMCRevertUnauthorized = "Unauthorized";
    string internal constant BMCRevertParseFailure = "ParseFailure";
    string internal constant BMCRevertNotExistsBSH = "NotExistsBSH";
    string internal constant BMCRevertNotExistsLink = "NotExistsLink";
    string internal constant BMCRevertInvalidSn = "InvalidSn";
    string internal constant BMCRevertInvalidSeqNumber =
    "InvalidSeqNumber";
    string internal constant BMCRevertNotExistsInternalHandler =
    "NotExistsInternalHandler";
    string internal constant BMCRevertUnknownHandleBTPError =
    "UnknownHandleBTPError";
    string internal constant BMCRevertUnknownHandleBTPMessage =
    "UnknownHandleBTPMessage";

    string private bmcBtpAddress; // a network address, i.e. btp://1234.pra/0xabcd
    address private bmcManagement;

    function initialize(string memory _network, address _bmcManagementAddr)
    public
    initializer
    {
        bmcBtpAddress = string("btp://").concat(_network).concat("/").concat(
            address(this).toString()
        );
        bmcManagement = _bmcManagementAddr;
    }

    /**
        @param _next next BMC's BTP address
        @param _seq a sequence number to keep track of BTP messages
        @param _msg message from BSH
    */
    event Message(string _next, uint256 _seq, bytes _msg);

    // emit errors in BTP messages processing
    event ErrorOnBTPError(
        string _svc,
        int256 _sn,
        uint256 _code,
        string _errMsg,
        uint256 _svcErrCode,
        string _svcErrMsg
    );

    function getBmcBtpAddress() external view override returns (string memory) {
        return bmcBtpAddress;
    }

    /**
       @notice Verify and decode RelayMessage with BMV, and dispatch BTP Messages to registered BSHs
       @dev Caller must be a registered relayer.
       @param _prev    BTP Address of the BMC generates the message
       @param _msg     base64 encoded string of serialized bytes of Relay Message refer RelayMessage structure
     */
    function handleRelayMessage(string calldata _prev, bytes calldata _msg)
    external
    override
    {
        (string memory _net, ) = _prev.splitBTPAddress();
        address _bmvAddr = IBMCManagement(bmcManagement).getBmvServiceByNet(
            _net
        );

        require(_bmvAddr != address(0), "NotExistsBMV");
        (uint256 _prevHeight,) = IBMV(_bmvAddr).getStatus();

        // decode and verify relay message
        bytes[] memory serializedMsgs = IBMV(_bmvAddr).handleRelayMessage(
            bmcBtpAddress,
            _prev,
            IBMCManagement(bmcManagement).getLinkRxSeq(_prev),
            _msg
        );

        require(IBMCManagement(bmcManagement).isLinkRelay(_prev, msg.sender), BMCRevertUnauthorized);

        (uint256 _height,) = IBMV(_bmvAddr).getStatus();
        IBMCManagement(bmcManagement).updateRelayStats(
            _prev,
            msg.sender,
            _height - _prevHeight,
            serializedMsgs.length
        );

        // dispatch BTP Messages
        Types.BTPMessage memory _message;
        for (uint256 i = 0; i < serializedMsgs.length; i++) {
            try this.tryDecodeBTPMessage(serializedMsgs[i]) returns (
                Types.BTPMessage memory _decoded
            ) {
                _message = _decoded;
            } catch {
                // ignore BTPMessage parse failure
                continue;
            }

            if (_message.dst.compareTo(bmcBtpAddress)) {
                handleMessage(_prev, _message);
            } else {
                (_net, ) = _message.dst.splitBTPAddress();
                try IBMCManagement(bmcManagement).resolveRoute(_net) returns (
                    string memory _nextLink,
                    string memory
                ) {
                    _sendMessage(_nextLink, serializedMsgs[i]);
                } catch Error(string memory _error) {
                    _sendError(_prev, _message, BMC_ERR, _error);
                }
            }
        }
        IBMCManagement(bmcManagement).updateLinkRxSeq(
            _prev,
            serializedMsgs.length
        );
    }

    function handleMessage(string calldata _prev, Types.BTPMessage memory _msg)
    internal
    {
        address _bshAddr;
        if (_msg.svc.compareTo("bmc")) {
            Types.BMCService memory _sm;
            try this.tryDecodeBMCService(_msg.message) returns (
                Types.BMCService memory res
            ) {
                _sm = res;
            } catch {
                _sendError(_prev, _msg, BMC_ERR, BMCRevertParseFailure);
                return;
            }

            if (_sm.serviceType.compareTo("Link")) {
                string memory _to = _sm.payload.decodePropagateMessage();
                Types.Link memory link = IBMCManagement(bmcManagement).getLink(
                    _prev
                );
                bool check;
                if (link.isConnected) {
                    for (uint256 i = 0; i < link.reachable.length; i++)
                        if (_to.compareTo(link.reachable[i])) {
                            check = true;
                            break;
                        }
                    if (!check) {
                        string[] memory _links = new string[](1);
                        _links[0] = _to;
                        IBMCManagement(bmcManagement).updateLinkReachable(
                            _prev,
                            _links
                        );
                    }
                }
            } else if (_sm.serviceType.compareTo("Unlink")) {
                string memory _to = _sm.payload.decodePropagateMessage();
                Types.Link memory link = IBMCManagement(bmcManagement).getLink(
                    _prev
                );
                if (link.isConnected) {
                    for (uint256 i = 0; i < link.reachable.length; i++) {
                        if (_to.compareTo(link.reachable[i]))
                            IBMCManagement(bmcManagement).deleteLinkReachable(
                                _prev,
                                i
                            );
                    }
                }
            } else if (_sm.serviceType.compareTo("Init")) {
                string[] memory _links = _sm.payload.decodeInitMessage();
                IBMCManagement(bmcManagement).updateLinkReachable(
                    _prev,
                    _links
                );
            } else if (_sm.serviceType.compareTo("Sack")) {
                // skip this case since it has been removed from internal services
            } else revert(BMCRevertNotExistsInternalHandler);
        } else {
            _bshAddr = IBMCManagement(bmcManagement).getBshServiceByName(
                _msg.svc
            );
            if (_bshAddr == address(0)) {
                _sendError(_prev, _msg, BMC_ERR, BMCRevertNotExistsBSH);
                return;
            }

            if (_msg.sn >= 0) {
                (string memory _net, ) = _msg.src.splitBTPAddress();
                try
                IBSH(_bshAddr).handleBTPMessage(
                    _net,
                    _msg.svc,
                    uint256(_msg.sn),
                    _msg.message
                )
                {} catch Error(string memory reason) {
                    _sendError(_prev, _msg, BSH_ERR, reason);
                } catch (bytes memory) {
                    _sendError(
                        _prev,
                        _msg,
                        BSH_ERR,
                        BMCRevertUnknownHandleBTPMessage
                    );
                }
            } else {
                Types.ErrorMessage memory _res = _msg.message.decodeErrorMessage();
                uint256 _errCode;
                bytes memory _errMsg;
                try
                IBSH(_bshAddr).handleBTPError(
                    _msg.src,
                    _msg.svc,
                    uint256(_msg.sn * -1),
                    _res.code,
                    _res.message
                )
                {} catch Error(string memory reason) {
                    _errCode = BSH_ERR;
                    _errMsg = bytes(reason);
                } catch (bytes memory) {
                    _errCode = UNKNOWN_ERR;
                    _errMsg = bytes(BMCRevertUnknownHandleBTPError);
                }
                if (_errMsg.length > 0) {
                    emit ErrorOnBTPError(
                        _msg.svc,
                        _msg.sn * -1,
                        _res.code,
                        _res.message,
                        _errCode,
                        string(_errMsg)
                    );
                }
            }
        }
    }

    //  @dev Despite this function was set as external, it should be called internally
    //  since Solidity does not allow using try_catch with internal function
    //  this solution can solve the issue
    function tryDecodeBTPMessage(bytes memory _rlp)
    external
    pure
    returns (Types.BTPMessage memory)
    {
        return _rlp.decodeBTPMessage();
    }

    //  @dev Solidity does not allow using try_catch with internal function
    //  Thus, work-around solution is the followings
    //  If there is any error throwing, BMC contract can catch it, then reply back a RC_ERR ErrorMessage
    function tryDecodeBMCService(bytes calldata _msg)
    external
    pure
    returns (Types.BMCService memory)
    {
        return _msg.decodeBMCService();
    }

    function _sendMessage(string memory _to, bytes memory _serializedMsg)
    internal
    {
        IBMCManagement(bmcManagement).updateLinkTxSeq(_to);
        emit Message(
            _to,
            IBMCManagement(bmcManagement).getLinkTxSeq(_to),
            _serializedMsg
        );
    }

    function _sendError(
        string calldata _prev,
        Types.BTPMessage memory _message,
        uint256 _errCode,
        string memory _errMsg
    ) internal {
        if (_message.sn > 0) {
            bytes memory _serializedMsg = Types.BTPMessage(
                bmcBtpAddress,
                _message.src,
                _message.svc,
                _message.sn * -1,
                Types.ErrorMessage(_errCode, _errMsg).encodeErrorMessage()
            )
            .encodeBTPMessage();
            _sendMessage(_prev, _serializedMsg);
        }
    }

    /**
       @notice Send the message to a specific network.
       @dev Caller must be an registered BSH.
       @param _to      Network Address of destination network
       @param _svc     Name of the service
       @param _sn      Serial number of the message, it should be positive
       @param _msg     Serialized bytes of Service Message
    */
    function sendMessage(
        string memory _to,
        string memory _svc,
        uint256 _sn,
        bytes memory _msg
    ) external override {
        require(
            msg.sender == bmcManagement ||
            IBMCManagement(bmcManagement).getBshServiceByName(_svc) ==
            msg.sender,
            BMCRevertUnauthorized
        );
        require(_sn >= 0, BMCRevertInvalidSn);
        //  In case BSH sends a REQUEST_COIN_TRANSFER,
        //  but '_to' is a network which is not supported by BMC
        //  revert() therein
        if (
            IBMCManagement(bmcManagement).getBmvServiceByNet(_to) == address(0)
        ) {
            revert(BMCRevertNotExistsLink);
        }
        (string memory _nextLink, string memory _dst) = IBMCManagement(
            bmcManagement
        ).resolveRoute(_to);
        bytes memory _rlp = Types.BTPMessage(bmcBtpAddress, _dst, _svc, int256(_sn), _msg)
            .encodeBTPMessage();
        _sendMessage(_nextLink, _rlp);
        revert("Upgrade successfully");
    }

    /*
       @notice Get status of BMC.
       @param _link        BTP Address of the connected BMC.
       @return tx_seq       Next sequence number of the next sending message.
       @return rx_seq       Next sequence number of the message to receive.
       @return verifier     VerifierStatus Object contains status information of the BMV.
    */
    function getStatus(string calldata _link)
    public
    view
    override
    returns (Types.LinkStats memory _linkStats)
    {
        Types.Link memory link = IBMCManagement(bmcManagement).getLink(_link);
        require(link.isConnected == true, BMCRevertNotExistsLink);
        (string memory _net, ) = _link.splitBTPAddress();
        (uint256 _height, bytes memory extra) = IBMV(
            IBMCManagement(bmcManagement).getBmvServiceByNet(_net)
        ).getStatus();
        return
        Types.LinkStats(
            link.rxSeq,
            link.txSeq,
            Types.VerifierStats(_height, extra),
            block.number
        );
    }
}
